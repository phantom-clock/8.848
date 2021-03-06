package kvraft

import (
	"../labgob"
	"../labrpc"
	"../raft"
	"bytes"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const Debug = 0

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		log.Printf(format, a...)
	}
	return
}

const (
	put = "Put"
	apd = "Append"
)

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	Cmd      string
	Key      string
	Value    string
	From     int
	CmdIndex int32
}

type KVServer struct {
	raft.DLock
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg
	dead    int32 // set by Kill()

	maxraftstate int // snapshot if log grows this big

	// Your definitions here.
	internalDB map[string]string
	waited     map[Op]chan struct{}
	mu         sync.Mutex

	reqIndex map[int]int32
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	kv.mu.Lock()
	defer kv.mu.Unlock()
	defer DPrintf("%d,get return:%+v", kv.me, reply)
	if _, leader := kv.rf.GetState(); !leader {
		reply.Err = ErrWrongLeader
		return
	}
	if kv.rf.PrevLog {
		kv.rf.Start(0)
		time.Sleep(100 * time.Millisecond)
	}

	v, existed := kv.internalDB[args.Key]
	if !existed {
		reply.Err = ErrNoKey
	} else {
		reply.Err = OK
		reply.Value = v
	}
}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	kv.mu.Lock()
	defer kv.mu.Unlock()
	reply.Err = OK
	if _, isLeader := kv.rf.GetState(); !isLeader {
		//DPrintf("%d refuse:not leader", kv.me)
		reply.Err = ErrWrongLeader
		return
	}

	v, existed := kv.reqIndex[args.From]
	if existed && v >= args.CmdIndex {
		DPrintf("duplicated request... %+v",args)
		return
	}

	//client是同步发送的,所以request index应该是连续的....

	DPrintf("%d accept", kv.me)
	op := Op{args.Op, args.Key, args.Value, args.From, args.CmdIndex}
	if _, ok := kv.waited[op]; !ok {
		kv.waited[op] = make(chan struct{}, 10)
	}
	kv.rf.Start(op)
	select {
	case <-kv.waited[op]:
		DPrintf("put rpc done,%+v", args)
		return
	case <-kv.rf.Ls.Done:
		DPrintf("put time out...")
		reply.Err = ErrTimeout
		return
	}
}

//
// the tester calls Kill() when a KVServer instance won't
// be needed again. for your convenience, we supply
// code to set rf.dead (without needing a lock),
// and a killed() method to test rf.dead in
// long-running loops. you can also add your own
// code to Kill(). you're not required to do anything
// about this, but it may be convenient (for example)
// to suppress debug output from a Kill()ed instance.
//
func (kv *KVServer) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *KVServer) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Raft to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
// the k/v server should snapshot when Raft's saved state exceeds maxraftstate bytes,
// in order to allow Raft to garbage-collect its log. if maxraftstate is -1,
// you don't need to snapshot.
// StartKVServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *KVServer {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.internalDB = make(map[string]string)
	kv.waited = make(map[Op]chan struct{})
	kv.reqIndex = make(map[int]int32)
	// You may need initialization code here.

	kv.applyCh = make(chan raft.ApplyMsg)

	go func() {
		for msg := range kv.applyCh {
			kv.Lock("apply to state machine")
			op, ok := msg.Command.(Op)
			if ok {
				DPrintf("%d's old value is %s,new op is %+v", kv.me, kv.internalDB[op.Key], msg)
				kv.applyMSG(op)
				go func() { kv.waited[op] <- struct{}{} }()
			}
			kv.Unlock()
		}
	}()
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)
	c := make(chan []byte)
	kv.rf.Chsnapshot = c
	go func() {
		for x := range c {
			DPrintf("%d,apply snapshot",kv.rf.Me)
			kv.Lock("unserialize")
			kv.UnserializeServer(x)
			kv.Unlock()
		}
	}()
	return kv
}
func (kv *KVServer) applyMSG(op Op) {
	v, existed := kv.reqIndex[op.From]
	if !existed {
		kv.reqIndex[op.From] = op.CmdIndex
	} else if v >= op.CmdIndex {
		return
	}
	kv.reqIndex[op.From] = op.CmdIndex

	if kv.maxraftstate != -1 && kv.rf.Persister.RaftStateSize() > kv.maxraftstate {
		s := kv.SerializeServer()
		DPrintf("do snapshot:1")
		kv.rf.DoSnapshot(s, op)
	}

	if op.Cmd == put {
		kv.internalDB[op.Key] = op.Value
	} else if op.Cmd == apd {
		v, ok := kv.internalDB[op.Key]
		if !ok {
			kv.internalDB[op.Key] = op.Value
		} else {
			kv.internalDB[op.Key] = v + op.Value
		}
	}
}
func (kv *KVServer) UnserializeServer(b []byte) {
	d := labgob.NewDecoder(bytes.NewBuffer(b))
	d.Decode(&kv.internalDB)
	d.Decode(kv.reqIndex)
	DPrintf("%d start unserialze..........", kv.rf.Me)
	for k, v := range kv.internalDB {
		DPrintf("%s,%s", k, v)
	}
}
func (kv *KVServer) SerializeServer() []byte {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(kv.internalDB)
	e.Encode(kv.reqIndex)
	return w.Bytes()
}
