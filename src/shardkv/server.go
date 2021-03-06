package shardkv

import "net"
import "fmt"
import "net/rpc"
import "time"
import "paxos"
import "sync"
import "sync/atomic"
import "log"
import "os"
import "syscall"
import "encoding/gob"
import "math/rand"
import "shardmaster"

const Debug = 0

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		fmt.Printf(format, a...)
	}
	return
}

const (
	Get    = "Get"
	Put    = "Put"
	Append = "Append"
	Reconf = "Reconf"
)

//
// Data structure for logging Get/Put/Append/Reconfigure ops
// using Paxos  
//
type Op struct {
	CID   string    // Client ID
	Seq   int       // Cleint Seq
	Op	  string
	Key   string
	Value string
	Extra interface{}
}

func (op *Op) IsSame(other* Op) bool {
	if op.Op == other.Op {
		if op.Op == Reconf {
			// Seq refers to config_num in 'Reconf' cases
			return op.Seq == other.Seq
		} 
		return op.CID == other.CID && op.Seq == other.Seq
	}
	return false
}

//
// Rep is compatible with GetReply and PutAppendReply
//     is used for simplifying the implemetation
//
type Rep struct {
	Err   Err
	Value string
}

//
// Key/value store & client states
//     these data will be transferred between replica groups
//     when the configuration is changed
//
type XState struct { 	
	// key-value store
	KVStore  map[string]string 
	//_________________________________________________________
	// client states for filtering duplicate ops

	// map client -> the Most Recent Request Seq of the client
	MRRSMap  map[string]int     	
	// map client -> the most recent apply to the client
	Replies  map[string]Rep
	//_________________________________________________________
}

func (xs *XState) Init() {
	xs.KVStore = map[string]string{}
	xs.MRRSMap = map[string]int{}
	xs.Replies = map[string]Rep{}
}

func (xs *XState) Update(other *XState) {
	for key, value := range other.KVStore {
		xs.KVStore[key] = value
	}

	for cli, seq := range other.MRRSMap {
		xseq := xs.MRRSMap[cli] 
		if xseq < seq {
			xs.MRRSMap[cli] = seq
			xs.Replies[cli] = other.Replies[cli]
		}
	}
}

func MakeXState() (*XState) {
	var xstate XState
	xstate.Init()
	return &xstate
}

type ShardKV struct {
	mu         sync.Mutex
	l          net.Listener
	me         int
	dead       int32 // for testing
	unreliable int32 // for testing
	sm         *shardmaster.Clerk
	px         *paxos.Paxos

	gid int64 // my replica group ID

	last_seq   int   // seq for next op to be applied
	seq        int   // next seq in paxos log

	config     shardmaster.Config
	
	xstate     XState
}

func (kv *ShardKV) logOperation(xop *Op) {
	seq := kv.seq

	wait_init := 10 * time.Millisecond

	DPrintf("----- server %d:%d logOperation %v\n", kv.gid, kv.me, xop)
	wait := wait_init
	for {
		fate, v := kv.px.Status(seq)
		if fate == paxos.Decided {
			op := v.(Op)
			DPrintf("----- server %d:%d : seq %d : %v\n", kv.gid, kv.me, seq, op)
			if xop.IsSame(&op) {
				break
			}			
			seq++
			wait = wait_init
		} else { // Pending
			DPrintf("----- server %d:%d starts a new paxos instance : %d %v\n", kv.gid, kv.me, seq, xop)
			kv.px.Start(seq, *xop)
			time.Sleep(wait)
			if wait < time.Second {
				wait *= 2
			}
		}
	}
	kv.seq = seq + 1
}

// 
// we let this func return the reply of the last Get/Put/Append op
// for simplifying our implementation of RPC Get/PutAppend 
//
func (kv *ShardKV) catchUp() (rep *Rep) {
	seq := kv.last_seq
	for seq < kv.seq {
		_, v := kv.px.Status(seq)
		op := v.(Op)
		if op.Op == Reconf {
			kv.config = kv.sm.Query(op.Seq)
			extra := op.Extra.(XState)
			kv.xstate.Update(&extra)
			DPrintf("doReconf : server %d:%d : config %d\n", kv.gid, kv.me, kv.config.Num)
		} else if op.Op == Put || op.Op == Append {
			rep = kv.doPutAppend(op.Op, op.Key, op.Value)
			kv.recordOperation(op.CID, op.Seq, rep)
		} else {
			rep = kv.doGet(op.Key)
			kv.recordOperation(op.CID, op.Seq, rep)
		}
		kv.px.Done(seq)
		seq++
	}
	kv.last_seq = seq
	return
}

func (kv *ShardKV) recordOperation(cid string, seq int, reply *Rep) {
	// we do not update the client state when ErrWrongGroup occurs
	if reply.Err != ErrWrongGroup {
		kv.xstate.MRRSMap[cid] = seq
		kv.xstate.Replies[cid] = *reply
	}
}

func (kv *ShardKV) filterDuplicate(cid string, seq int) (*Rep, bool) {
	last_seq := kv.xstate.MRRSMap[cid]
	if seq < last_seq { 
		return nil, true 
	} else if seq == last_seq {
		rep := kv.xstate.Replies[cid]
		return &rep, true
	} 
	return nil, false
}

func (kv *ShardKV) doGet(key string) (*Rep) {
	var rep Rep
	if kv.gid != kv.config.Shards[key2shard(key)] {
		DPrintf("doGet       : ErrWrongGroup : server %d:%d : key %s\n", kv.gid, kv.me, key)
		DPrintf("------------- config : %v\n", kv.config)
		rep.Err = ErrWrongGroup
	} else {
		value, ok := kv.xstate.KVStore[key]
		DPrintf("doGet : server %d:%d : key %s : value %s\n", 
			kv.gid, kv.me, key, value)
		if ok {
			rep.Err, rep.Value = OK, value
		} else {
			rep.Err = ErrNoKey
		}
	}
	return &rep
}

func (kv *ShardKV) doPutAppend(op string, key string, value string) (*Rep) {
	var rep Rep
	if kv.gid != kv.config.Shards[key2shard(key)] {
		DPrintf("doPutAppend : ErrWrongGroup : server %d:%d : key %s\n", kv.gid, kv.me, key)
		DPrintf("------------- config : %v\n", kv.config)
		rep.Err = ErrWrongGroup
	} else {
		value1 := kv.xstate.KVStore[key]
		if op == Put {
			kv.xstate.KVStore[key] = value
		} else if op == Append {
			kv.xstate.KVStore[key] += value
		}
		DPrintf("doPutAppend : server %d:%d : op %s : key %s : value %s->%s\n", 
		kv.gid, kv.me, op, key, value1, kv.xstate.KVStore[key])
		rep.Err = OK
	}
	return &rep
}
	
func (kv *ShardKV) Get(args *GetArgs, reply *GetReply) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	DPrintf("RPC Get : server %d:%d : cleint %s : seq %d : key %s\n", 
		kv.gid, kv.me, args.CID, args.Seq, args.Key)
	
	// we catch up to update the client states (filters actually)
	kv.catchUp()

	rp, yes := kv.filterDuplicate(args.CID, args.Seq)
	if yes {
		DPrintf("RPC Get : server %d:%d : dup-op detected : %v\n", kv.gid, kv.me, args)
		if rp != nil {
			reply.Err, reply.Value = rp.Err, rp.Value
		}
		return nil
	}

	xop := &Op{CID:args.CID, Seq:args.Seq, Op:Get, Key:args.Key}
	kv.logOperation(xop)

	rep := kv.catchUp()
	reply.Err, reply.Value = rep.Err, rep.Value

	return nil
}


// RPC handler for client Put and Append requests
func (kv *ShardKV) PutAppend(args *PutAppendArgs, reply *PutAppendReply) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	
	DPrintf("RPC PutAppend : server %d:%d : cleint %s : seq %d : op %s : key %s :value %s\n", 
		kv.gid, kv.me, args.CID, args.Seq, args.Op, args.Key, args.Value)

	kv.catchUp()

	rp, yes := kv.filterDuplicate(args.CID, args.Seq) 
	if yes {
		DPrintf("RPC PutAppend : server %d:%d : dup-op detected %v\n", kv.gid, kv.me, args)
		if rp != nil {
			reply.Err = rp.Err
		}
		return nil
	}
	
	xop := &Op{CID:args.CID, Seq:args.Seq, Op:args.Op, Key:args.Key, Value:args.Value}
	kv.logOperation(xop)
	
	rep := kv.catchUp()
	reply.Err = rep.Err

	return nil
}

func (kv *ShardKV) reconfigure(config *shardmaster.Config) bool {
	//DPrintf("----- server %d:%d : reconfigure %v\n", kv.gid, kv.me, config)
	
	// we catch up to ensure that kv.config.Num equals config.Num - 1
	kv.catchUp()

	xstate := MakeXState()
	for shard := 0; shard < shardmaster.NShards; shard++ {
		gid := kv.config.Shards[shard]
		if config.Shards[shard] == kv.gid && gid != 0 && gid != kv.gid {
		 	ret := kv.requestShard(gid, shard)
			if ret == nil { 
				return false
			}
			xstate.Update(ret)
		}
	}
	xop := &Op{Seq:config.Num, Op:Reconf, Extra:*xstate}
	kv.logOperation(xop)

	return true
}

func (kv *ShardKV) requestShard(gid int64, shard int) (*XState) {
	DPrintf("----- server %d:%d : requestShard %d:%d\n", kv.gid, kv.me, gid, shard)

	for _, server := range kv.config.Groups[gid] {
		args := &TransferStateArgs{}
		args.ConfigNum, args.Shard = kv.config.Num, shard
		var reply TransferStateReply
		ok := call(server, "ShardKV.TransferState", args, &reply)
		if ok && reply.Err == OK {
			return &reply.XState
		}
	}
	DPrintf("----- server %d:%d : requestShard FAIL %v\n", kv.gid, kv.me, kv.config)
	return nil
}

func (kv *ShardKV) TransferState(args *TransferStateArgs, reply *TransferStateReply) error {
	DPrintf("RPC TransferState : Deadlock ? : server %d:%d ConfigNum %d vs args.ConfigNum %d\n", 
		kv.gid, kv.me, kv.config.Num, args.ConfigNum)
	
	// we check if we have older config than the client-server's 
	// it's ok to use kv.config.Num here :)
	if kv.config.Num < args.ConfigNum {
		reply.Err = ErrNotReady
		return nil
	} 
	
	kv.mu.Lock()
	defer kv.mu.Unlock()

	DPrintf("RPC TransferState : server %d:%d : args %v\n", kv.gid, kv.me, args)

	reply.XState.Init()
	
	for key := range kv.xstate.KVStore {
		if key2shard(key) == args.Shard {
			value := kv.xstate.KVStore[key]
			reply.XState.KVStore[key] = value
		}
	}
	for client := range kv.xstate.MRRSMap {
		reply.XState.MRRSMap[client] = kv.xstate.MRRSMap[client] 
		reply.XState.Replies[client] = kv.xstate.Replies[client]
	}

	reply.Err = OK
	return nil
}

//
// Ask the shardmaster if there's a new configuration;
// if so, re-configure.
//
func (kv *ShardKV) tick() {
	DPrintf("server %d:%d ---*--- tick ---*---\n", kv.gid, kv.me)
	kv.mu.Lock()
	defer kv.mu.Unlock()
	
	// we catch up, in case we would log same ops as before
	kv.catchUp()

	latest_config := kv.sm.Query(-1)
	for n := kv.config.Num + 1; n <= latest_config.Num; n++ {
		config := kv.sm.Query(n)
		if !kv.reconfigure(&config) {
			break
		}
	}
}

// tell the server to shut itself down.
// please don't change these two functions.
func (kv *ShardKV) kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.l.Close()
	kv.px.Kill()
}

// call this to find out if the server is dead.
func (kv *ShardKV) isdead() bool {
	return atomic.LoadInt32(&kv.dead) != 0
}

// please do not change these two functions.
func (kv *ShardKV) Setunreliable(what bool) {
	if what {
		atomic.StoreInt32(&kv.unreliable, 1)
	} else {
		atomic.StoreInt32(&kv.unreliable, 0)
	}
}

func (kv *ShardKV) isunreliable() bool {
	return atomic.LoadInt32(&kv.unreliable) != 0
}

//
// Start a shardkv server.
// gid is the ID of the server's replica group.
// shardmasters[] contains the ports of the
//   servers that implement the shardmaster.
// servers[] contains the ports of the servers
//   in this replica group.
// Me is the index of this server in servers[].
//
func StartServer(gid int64, shardmasters []string,
	servers []string, me int) *ShardKV {
	gob.Register(Op{})
	gob.Register(XState{})

	kv := new(ShardKV)
	kv.me = me
	kv.gid = gid
	kv.sm = shardmaster.MakeClerk(shardmasters)

	// Your initialization code here.
	// Don't call Join().

	rpcs := rpc.NewServer()
	rpcs.Register(kv)

	kv.px = paxos.Make(servers, me, rpcs)

	kv.xstate.Init()

	os.Remove(servers[me])
	l, e := net.Listen("unix", servers[me])
	if e != nil {
		log.Fatal("listen error: ", e)
	}
	kv.l = l

	// please do not change any of the following code,
	// or do anything to subvert it.

	go func() {
		for kv.isdead() == false {
			conn, err := kv.l.Accept()
			if err == nil && kv.isdead() == false {
				if kv.isunreliable() && (rand.Int63()%1000) < 100 {
					// discard the request.
					conn.Close()
				} else if kv.isunreliable() && (rand.Int63()%1000) < 200 {
					// process the request but force discard of reply.
					c1 := conn.(*net.UnixConn)
					f, _ := c1.File()
					err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
					if err != nil {
						fmt.Printf("shutdown: %v\n", err)
					}
					go rpcs.ServeConn(conn)
				} else {
					go rpcs.ServeConn(conn)
				}
			} else if err == nil {
				conn.Close()
			}
			if err != nil && kv.isdead() == false {
				fmt.Printf("ShardKV(%v) accept: %v\n", me, err.Error())
				kv.kill()
			}
		}
	}()

	go func() {
		for kv.isdead() == false {
			kv.tick()
			time.Sleep(250 * time.Millisecond)
		}
	}()

	return kv
}

