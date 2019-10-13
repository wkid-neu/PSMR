package paxos

//
// Paxos library, to be included in an application.
// Multiple applications will run, each including
// a Paxos peer.
//
// Manages a sequence of agreed-on values.
// The set of peers is fixed.
// Copes with network failures (partition, msg loss, &c).
// Does not store anything persistently, so cannot handle crash+restart.
//
// The application interface:
//
// px = paxos.Make(peers []string, me string)
// px.Start(seq int, v interface{}) -- start agreement on new instance
// px.Status(seq int) (decided bool, v interface{}) -- get info about an instance
// px.Done(seq int) -- ok to forget all instances <= seq
// px.Max() int -- highest instance seq known, or -1
// px.Min() int -- instances before this seq have been forgotten
//

import "net"
import "net/rpc"
import "log"
import "os"
import "syscall"
import "sync"
import "fmt"
import "math/rand"
import "time"

type ErrCode int
type State int

const (
	ErrOK ErrCode = ErrCode(iota)
	ErrNetwork
	ErrRejected
)

const (
	StateUndecided State = State(iota)
	StateDecided
)

type Paxos struct {
	mu         sync.RWMutex
	l          net.Listener
	dead       bool
	unreliable bool
	rpcCount   int
	peers      []string
	me         int // index into peers[]

	value    interface{}
	instance map[int]*PaxosInst
	done     []int
}

type PrepareReq struct {
	Seq   int
	Stamp int64
}

type PrepareRsp struct {
	Code  ErrCode
	Value interface{}
	Stamp int64
}

type CommitReq struct {
	Seq   int
	Value interface{}
	Stamp int64
}

type CommitRsp struct {
	Code ErrCode
}

type DecideReq struct {
	Seq   int
	Value interface{}
	Svrid int
	Done  int
}

type DecideRsp struct {
}

type PaxosInst struct {
	status  State
	value   interface{}
	platest int64
	alatest int64
}

//
// call() sends an RPC to the rpcname handler on server srv
// with arguments args, waits for the reply, and leaves the
// reply in reply. the reply argument should be a pointer
// to a reply structure.
//
// the return value is true if the server responded, and false
// if call() was not able to contact the server. in particular,
// the replys contents are only valid if call() returned true.
//
// you should assume that call() will time out and return an
// error after a while if it does not get a reply from the server.
//
// please use call() to send all RPCs, in client.go and server.go.
// please do not change this function.
//
func call(srv string, name string, args interface{}, reply interface{}) bool {
	c, err := rpc.Dial("unix", srv)
	//c, err := rpc.Dial("tcp", srv)
	if err != nil {
		err1 := err.(*net.OpError)
		if err1.Err != syscall.ENOENT && err1.Err != syscall.ECONNREFUSED {
			fmt.Printf("paxos Dial() failed: %v\n", err1)
		}
		return false
	}
	defer c.Close()

	err = c.Call(name, args, reply)
	if err == nil {
		return true
	}

	fmt.Println(err)
	return false
}

func (px *Paxos) AcceptPrepare(req *PrepareReq, rsp *PrepareRsp) error {
	px.mu.Lock()
	defer px.mu.Unlock()
	inst, ok := px.instance[req.Seq]
	if !ok {
		//还没有decide，因此需要初始化一下
		inst = &PaxosInst{StateUndecided, nil, req.Stamp, 0}
		px.instance[req.Seq] = inst
		rsp.Code = ErrOK

	} else if inst.platest <= req.Stamp {
		// 如果本acceptor的时间戳比proposer的时间戳小，acceptor接受你的提议
		//inst.platest = req.Stamp
		px.instance[req.Seq].platest = req.Stamp
		rsp.Code = ErrOK
	} else {
		rsp.Code = ErrRejected
	}

	rsp.Value = inst.value
	rsp.Stamp = inst.alatest
	return nil
}

func (px *Paxos) AcceptCommit(req *CommitReq, rsp *CommitRsp) error {
	px.mu.Lock()
	defer px.mu.Unlock()
	inst, ok := px.instance[req.Seq]
	if !ok {
		// 有些acceptor还没来的及进入prepare阶段直接进入了commit阶段
		//inst = &PaxosInst{StateUndecided, req.Value, req.Stamp, req.Stamp}
		inst = &PaxosInst{StateUndecided, req.Value, req.Stamp, 0}
		px.instance[req.Seq] = inst
		rsp.Code = ErrOK
	} else if inst.platest <= req.Stamp {
		rsp.Code = ErrOK
		inst.alatest = req.Stamp
		//inst.alatest = 0
		inst.value = req.Value
		inst.platest = req.Stamp
		//inst.status = StateUndecided
	} else {
		rsp.Code = ErrRejected
	}

	return nil
}

func (px *Paxos) AcceptDecided(req *DecideReq, rsp *DecideRsp) error {
	px.mu.Lock()
	defer px.mu.Unlock()
	inst, ok := px.instance[req.Seq]
	if !ok {
		inst = &PaxosInst{StateDecided, req.Value, 0, 0}
		px.instance[req.Seq] = inst
	} else {
		inst.status = StateDecided
		inst.value = req.Value
	}

	px.done[req.Svrid] = req.Done
	return nil
}

func (px *Paxos) sendPrepare(seq int, v interface{}) (bool, int64, interface{}) {

	ch := make(chan PrepareRsp, len(px.peers))
	timestamp := time.Now().UnixNano()
	for i, p := range px.peers {
		if i == px.me {
			go func() {
				rsp := PrepareRsp{}
				px.AcceptPrepare(&PrepareReq{seq, timestamp}, &rsp)
				ch <- rsp
			}()
			continue
		}

		go func(host string) {
			rsp := PrepareRsp{}

			if !call(host, "Paxos.AcceptPrepare",
				&PrepareReq{seq, timestamp}, &rsp) {
				rsp.Code = ErrNetwork
			}
			ch <- rsp
		}(p)
	}
	agreed := 0
	highest := int64(0)
	nv := interface{}(nil)
	for i := 0; i < len(px.peers); i++ {
		rsp := <-ch
		if rsp.Code == ErrOK {
			agreed++
			if rsp.Stamp > highest {
				nv = rsp.Value
				highest = rsp.Stamp
			}
		}

	}

	if agreed <= len(px.peers)/2 {
		return false, timestamp, nv
	}
	return true, timestamp, nv
}

func (px *Paxos) sendCommit(seq int, v interface{}, timestamp int64) bool {
	cch := make(chan CommitRsp, len(px.peers)) // 为什么要定义一个这么长的管道？？？
	for i := 0; i < len(px.peers); i++ {
		if i == px.me {
			go func() {
				rsp := CommitRsp{}
				px.AcceptCommit(&CommitReq{seq, v, timestamp}, &rsp)
				cch <- rsp
			}()
			continue
		}
		go func(host string) {
			rsp := CommitRsp{}
			if !call(host, "Paxos.AcceptCommit",
				&CommitReq{seq, v, timestamp}, &rsp) {
				rsp.Code = ErrNetwork
			}
			cch <- rsp
		}(px.peers[i])
	}
	accept := 0
	for i := 0; i < len(px.peers); i++ {
		rsp := <-cch
		if rsp.Code == ErrOK {
			accept++
		}
	}
	if accept <= len(px.peers)/2 {
		return false
	}
	return true
}

func (px *Paxos) sendDecision(seq int, v interface{}) {
	cch := make(chan DecideRsp, len(px.peers))
	for i := 0; i < len(px.peers); i++ {
		if i == px.me {
			go func() {
				rsp := DecideRsp{}
				px.AcceptDecided(&DecideReq{seq, v, px.me, px.done[px.me]}, &rsp)
				cch <- rsp
			}()
			continue
		}
		go func(host string) {
			rsp := DecideRsp{}
			call(host, "Paxos.AcceptDecided", &DecideReq{seq, v, px.me, px.done[px.me]}, &rsp)
			cch <- rsp
		}(px.peers[i])
	}
	for i := 0; i < len(px.peers); i++ {
		<-cch
	}
}

//
// the application wants paxos to start agreement on
// instance seq, with proposed value v.
// Start() returns right away; the application will
// call Status() to find out if/when agreement
// is reached.
//
func (px *Paxos) Start(seq int, v interface{}) {
	// Your code here.
	if seq < px.Min() {
		return
	}
	go func() {
		for {
			// ok是代表已经有大多数的已经prepare好了
			// nv代表proposer收到的prepare消息中的最大版本号。当然也有可能为空
			// t是时间戳
			ok, t, nv := px.sendPrepare(seq, v)
			if !ok {
				// 如果不ok，一直发，
				continue
			} else if nv != nil {
				//我提议的这个v成了别人的v？？？
				v = nv
			}
			if ok := px.sendCommit(seq, v, t); !ok {
				continue
			}
			px.sendDecision(seq, v)
			break
		}
	}()

}

//
// the application on this machine is done with
// all instances <= seq.
//
// see the comments for Min() for more explanation.
//
func (px *Paxos) Done(seq int) {
	if seq >= px.done[px.me] {
		px.done[px.me] = seq
	}
	// Your code here.
}

//
// the application wants to know the
// highest instance sequence known to
// this peer.
//
func (px *Paxos) Max() int {
	// Your code here.
	px.mu.RLock()
	defer px.mu.RUnlock()
	ans := 0

	for i, _ := range px.instance {
		if i > ans {
			ans = i
		}
	}
	return ans
}

//
// Min() should return one more than the minimum among z_i,
// where z_i is the highest number ever passed
// to Done() on peer i. A peers z_i is -1 if it has
// never called Done().
//
// Paxos is required to have forgotten all information
// about any instances it knows that are < Min().
// The point is to free up memory in long-running
// Paxos-based servers.
//
// Paxos peers need to exchange their highest Done()
// arguments in order to implement Min(). These
// exchanges can be piggybacked on ordinary Paxos
// agreement protocol messages, so it is OK if one
// peers Min does not reflect another Peers Done()
// until after the next instance is agreed to.
//
// The fact that Min() is defined as a minimum over
// *all* Paxos peers means that Min() cannot increase until
// all peers have been heard from. So if a peer is dead
// or unreachable, other peers Min()s will not increase
// even if all reachable peers call Done. The reason for
// this is that when the unreachable peer comes back to
// life, it will need to catch up on instances that it
// missed -- the other peers therefor cannot forget these
// instances.
//
func (px *Paxos) Min() int {
	// You code here.
	px.mu.Lock()
	defer px.mu.Unlock()
	ans := 0x7fffffff
	//fmt.Println(px.done)
	for _, d := range px.done {
		if ans > d {
			ans = d
		}
	}
	if len(px.done) == 0 {
		ans = 0
	}
	for seq, inst := range px.instance {
		if seq < ans && inst.status == StateDecided {
			delete(px.instance, seq)
		}
	}
	return ans + 1
}

//
// the application wants to know whether this
// peer thinks an instance has been decided,
// and if so what the agreed value is. Status()
// should just inspect the local peer state;
// it should not contact other Paxos peers.
//
func (px *Paxos) Status(seq int) (bool, interface{}) {
	// Your code here.
	px.mu.RLock()
	defer px.mu.RUnlock()
	if inst, ok := px.instance[seq]; ok && inst.status == StateDecided {
		return true, inst.value
	}
	return false, nil
}

//
// tell the peer to shut itself down.
// for testing.
// please do not change this function.
//
func (px *Paxos) Kill() {
	px.dead = true
	if px.l != nil {
		px.l.Close()
	}
}

//
// the application wants to create a paxos peer.
// the ports of all the paxos peers (including this one)
// are in peers[]. this servers port is peers[me].
//
func Make(peers []string, me int, rpcs *rpc.Server) *Paxos {
	px := &Paxos{value: nil}
	px.peers = peers
	px.me = me
	px.done = make([]int, len(peers))
	px.instance = make(map[int]*PaxosInst)
	for i := 0; i < len(peers); i++ {
		//fmt.Println(px.done[i])
		px.done[i] = -1
	}
	// Your initialization code here.

	if rpcs != nil {
		// caller will create socket &c
		rpcs.Register(px)
	} else {
		rpcs = rpc.NewServer()
		rpcs.Register(px)

		// prepare to receive connections from clients.
		// change "unix" to "tcp" to use over a network.
		os.Remove(peers[me]) // only needed for "unix"
		l, e := net.Listen("unix", peers[me])
		if e != nil {
			log.Fatal("listen error: ", e)
		}
		px.l = l

		// please do not change any of the following code,
		// or do anything to subvert it.

		// create a thread to accept RPC connections
		go func() {
			for px.dead == false {
				conn, err := px.l.Accept()
				if err == nil && px.dead == false {
					if px.unreliable && (rand.Int63()%1000) < 100 {
						// discard the request.
						// 拒绝请求
						conn.Close()
					} else if px.unreliable && (rand.Int63()%1000) < 200 {
						// process the request but force discard of reply.
						// 处理请求但是不回应
						c1 := conn.(*net.UnixConn)
						f, _ := c1.File()
						err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
						if err != nil {
							fmt.Printf("shutdown: %v\n", err)
						}
						px.rpcCount++
						go rpcs.ServeConn(conn)
					} else {
						px.rpcCount++
						go rpcs.ServeConn(conn)
					}
				} else if err == nil {
					conn.Close()
				}
				if err != nil && px.dead == false {
					fmt.Printf("Paxos(%v) accept: %v\n", me, err.Error())
				}
			}
		}()
	}

	return px
}