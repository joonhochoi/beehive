package beehive

import (
	"fmt"
	"net"
	"net/rpc"
	"sync"
	"time"

	etcdraft "github.com/kandoo/beehive/Godeps/_workspace/src/github.com/coreos/etcd/raft"
	"github.com/kandoo/beehive/Godeps/_workspace/src/github.com/golang/glog"
	"github.com/kandoo/beehive/Godeps/_workspace/src/golang.org/x/net/context"
	bhgob "github.com/kandoo/beehive/gob"
	"github.com/kandoo/beehive/raft"
)

const (
	maxWait = 8 * time.Second
	minWait = 50 * time.Millisecond
)

// HiveState represents the state of a hive.
type HiveState struct {
	ID    uint64     `json:"id"`    // ID is the ID of the hive.
	Addr  string     `json:"addr"`  // Addr is the hive's address.
	Peers []HiveInfo `json:"peers"` // Peers of the hive.
}

type rpcBackoffError struct {
	Until time.Time
}

func (e *rpcBackoffError) Error() string {
	return fmt.Sprintf("rpc-client: backoff until %v", e.Until)
}

func (e *rpcBackoffError) Temporary() bool { return true }
func (e *rpcBackoffError) Timeout() bool   { return true }

func isBackoffError(err error) bool {
	_, ok := err.(*rpcBackoffError)
	return ok
}

type dialTry struct {
	sync.Mutex
	next  time.Time
	wait  time.Duration
	tries uint64
}

type clientBackoff struct {
	client  *rpcClient
	backoff time.Time
}

func (cb clientBackoff) isSet() bool {
	return cb == clientBackoff{}
}

type rpcClientPool struct {
	sync.RWMutex
	hive *hive

	hiveClients map[uint64]*rpcClient
	beeClients  map[uint64]*rpcClient

	retries map[uint64]*dialTry
}

func newRPCClientPool(h *hive) *rpcClientPool {
	return &rpcClientPool{
		hive:        h,
		hiveClients: make(map[uint64]*rpcClient),
		beeClients:  make(map[uint64]*rpcClient),
		retries:     make(map[uint64]*dialTry),
	}
}

func (p *rpcClientPool) stop() {
	p.Lock()
	defer p.Unlock()

	for _, client := range p.hiveClients {
		client.stop()
	}
}

func (p *rpcClientPool) shouldReset(err error) bool {
	if err == nil {
		return false
	}

	if err == rpc.ErrShutdown {
		return true
	}

	nerr, ok := err.(net.Error)
	return ok && !nerr.Temporary()
}

func (p *rpcClientPool) sendRaft(batch *raft.Batch, r raft.Reporter) error {
	client, err := p.hiveClient(batch.To)
	if err != nil {
		report(err, batch, r)
		return err
	}

	if err = client.sendRaft(batch, r); p.shouldReset(err) {
		p.resetHiveClient(batch.To, client)
	}
	return err
}

// sendMsg tries to send the messages to all the bees, and will return the last
// error.
//
// It is preferred store and reuse the beeClient to relay messages.
func (p *rpcClientPool) sendMsg(msgs []msg) (err error) {
	mm := make(map[uint64][]msg)
	for _, msg := range msgs {
		mm[msg.To()] = append(mm[msg.To()], msg)
	}

	for b, bmsgs := range mm {
		client, berr := p.beeClient(b)
		if berr != nil {
			err = berr
			continue
		}

		if berr = client.sendMsg(bmsgs); p.shouldReset(berr) {
			p.resetBeeClient(b, client)
			err = berr
		}
	}

	return err
}

func (p *rpcClientPool) sendCmd(cmd cmd) (res interface{}, err error) {
	client, err := p.hiveClient(cmd.Hive)
	if err != nil {
		return nil, err
	}

	if res, err = client.sendCmd(cmd); p.shouldReset(err) {
		p.resetHiveClient(cmd.Hive, client)
	}
	return
}

func (p *rpcClientPool) lookupHive(hive uint64) (client *rpcClient, ok bool) {
	p.RLock()
	client, ok = p.hiveClients[hive]
	p.RUnlock()
	return
}

func (p *rpcClientPool) setHive(hive uint64, client *rpcClient) {
	p.Lock()
	p.hiveClients[hive] = client
	p.Unlock()
}

func (p *rpcClientPool) deleteHive(hive uint64) {
	p.Lock()
	delete(p.hiveClients, hive)
	p.Unlock()
}

func (p *rpcClientPool) lookupRetry(hive uint64) (t *dialTry) {
	p.Lock()
	t, ok := p.retries[hive]
	if !ok {
		t = &dialTry{wait: minWait}
		p.retries[hive] = t
	}
	p.Unlock()
	return
}

func (p *rpcClientPool) setRetry(hive uint64, t *dialTry) {
	p.Lock()
	p.retries[hive] = t
	p.Unlock()
}

func (p *rpcClientPool) hiveClient(hive uint64) (client *rpcClient, err error) {
	c, ok := p.lookupHive(hive)
	if ok {
		return c, nil
	}

	return p.resetHiveClient(hive, nil)
}

func (p *rpcClientPool) resetHiveClient(hive uint64, prev *rpcClient) (
	client *rpcClient, err error) {

	client, ok := p.lookupHive(hive)
	if ok && client != prev {
		return
	}

	p.deleteHive(hive)
	if client, err = p.newClient(hive); err != nil {
		return
	}

	p.setHive(hive, client)
	return
}

func (p *rpcClientPool) newClient(hive uint64) (client *rpcClient, err error) {
	t := p.lookupRetry(hive)

	t.Lock()
	defer t.Unlock()

	// 2nd check might be successful.
	client, ok := p.lookupHive(hive)
	if ok {
		return client, nil
	}

	now := time.Now()
	if !now.After(t.next) {
		return nil, &rpcBackoffError{Until: t.next}
	}

	i, err := p.hive.registry.hive(hive)
	if err != nil {
		return nil, err
	}

	if client, err = newRPCClient(i.Addr); err != nil {
		// contention here.
		t.tries++
		t.wait *= 2
		if t.wait > maxWait {
			t.wait = maxWait
		}
		t.next = now.Add(t.wait)
		p.setRetry(hive, t)
		return nil, err
	}

	t.wait = 1 * time.Second
	t.next = now
	p.setRetry(hive, t)
	p.setHive(hive, client)
	return client, nil
}

func (p *rpcClientPool) beeClient(bee uint64) (client *rpcClient, err error) {
	i, err := p.hive.bee(bee)
	if err != nil {
		return
	}

	return p.hiveClient(i.Hive)
}

func (p *rpcClientPool) resetBeeClient(bee uint64, prevClient *rpcClient) (
	client *rpcClient, err error) {

	i, err := p.hive.bee(bee)
	if err != nil {
		return
	}

	return p.resetHiveClient(i.Hive, prevClient)
}

type rpcClient struct {
	addr string

	cmd  *rpc.Client
	msg  *rpc.Client
	raft *rpc.Client
	prio *rpc.Client
}

func (c rpcClient) String() string {
	return fmt.Sprintf("rpc client to %s", c.addr)
}

func newRPCClient(addr string) (client *rpcClient, err error) {
	client = &rpcClient{
		addr: addr,
	}

	cmdConn, err := net.DialTimeout("tcp", addr, maxWait)
	if err != nil {
		return nil, err
	}
	client.cmd = rpc.NewClient(cmdConn)

	raftConn, err := net.DialTimeout("tcp", addr, maxWait)
	if err != nil {
		client.raft = client.cmd
	} else {
		client.raft = rpc.NewClient(raftConn)
	}

	prioConn, err := net.DialTimeout("tcp", addr, maxWait)
	if err != nil {
		client.prio = client.raft
	} else {
		client.prio = rpc.NewClient(prioConn)
	}

	msgConn, err := net.DialTimeout("tcp", addr, maxWait)
	if err != nil {
		client.msg = client.cmd
	} else {
		client.msg = rpc.NewClient(msgConn)
	}

	return client, nil
}

func (c *rpcClient) sendMsg(msgs []msg) error {
	var f struct{}
	glog.V(3).Infof("%v sends %v messages", c, len(msgs))
	return c.msg.Call("rpcServer.EnqueMsg", msgs, &f)
}

func (c *rpcClient) sendCmd(cm cmd) (res interface{}, err error) {
	glog.V(3).Infof("%v sends %v", c, cm)
	r := make([]cmdResult, 1)
	err = c.cmd.Call("rpcServer.ProcessCmd", []cmd{cm}, &r)
	if err != nil {
		return
	}
	return r[0].Data, r[0].Err
}

func snapStatus(err error) (ss etcdraft.SnapshotStatus) {
	if err != nil {
		ss = etcdraft.SnapshotFailure
	} else {
		ss = etcdraft.SnapshotFinish
	}
	return
}

func unreachable(err error) bool {
	return err != nil
}

func report(err error, batch *raft.Batch, r raft.Reporter) {
	for g, msgs := range batch.Messages {
		if unreachable(err) {
			r.ReportUnreachable(batch.To, g)
		}

		for _, msg := range msgs {
			if !etcdraft.IsEmptySnap(msg.Snapshot) {
				r.ReportSnapshot(batch.To, g, snapStatus(err))
			}
		}
	}
}

func (c *rpcClient) sendRaft(batch *raft.Batch, r raft.Reporter) (err error) {
	glog.V(3).Infof("%v sends a raft batch", c)
	var dummy bool
	if batch.Priority == raft.High {
		err = c.prio.Call("rpcServer.ProcessRaft", batch, &dummy)
	} else {
		err = c.raft.Call("rpcServer.ProcessRaft", batch, &dummy)
	}
	report(err, batch, r)
	return err
}

func (c *rpcClient) hiveState() (state HiveState, err error) {
	err = c.cmd.Call("rpcServer.HiveState", struct{}{}, &state)
	return
}

func getHiveState(addr string) (state HiveState, err error) {
	client, err := newRPCClient(addr)
	if err != nil {
		return
	}
	defer client.stop()
	return client.hiveState()
}

func (c *rpcClient) stop() {
	c.cmd.Close()
	c.msg.Close()
	c.raft.Close()
	c.prio.Close()
}

type rpcServer struct {
	h *hive
}

func newRPCServer(h *hive) *rpcServer {
	return &rpcServer{
		h: h,
	}
}

func (s *rpcServer) HiveState(dummy struct{}, state *HiveState) error {
	*state = HiveState{
		ID:    s.h.ID(),
		Addr:  s.h.config.Addr,
		Peers: s.h.registry.hives(),
	}
	return nil
}

func (s *rpcServer) ProcessCmd(cmds []cmd, res *[]cmdResult) error {
	if len(cmds) == 0 {
		return nil
	}

	*res = make([]cmdResult, len(cmds))

	chs := make([]chan cmdResult, 0, len(cmds))
	for _, c := range cmds {
		ch := make(chan cmdResult, 1)
		chs = append(chs, ch)

		if c.Hive != Nil && c.Hive != s.h.ID() {
			ch <- cmdResult{
				Err: bhgob.Errorf("rpc-server: %v receives command to %v", s.h, c.Hive),
			}
			continue
		}

		var ctrlCh chan cmdAndChannel
		if c.App == "" {
			glog.V(3).Infof("%v handles command to hive: %v", s.h, c)
			ctrlCh = s.h.ctrlCh
		} else {
			a, ok := s.h.app(c.App)
			if !ok {
				ch <- cmdResult{
					Err: bhgob.Errorf("rpc-server: %v cannot find app %v", s.h, c.App),
				}
				continue
			}

			glog.V(3).Infof("%v handles command to app %v: %v", s.h, a, c)
			if c.Bee == Nil {
				ctrlCh = a.qee.ctrlCh
			} else {
				b, ok := a.qee.beeByID(c.Bee)
				if !ok {
					ch <- cmdResult{
						Err: bhgob.Errorf("rpc-server: %v cannot find bee %v", a.qee,
							c.Bee),
					}
					continue
				}
				ctrlCh = b.ctrlCh
			}
		}

		ctrlCh <- cmdAndChannel{
			cmd: c,
			ch:  ch,
		}
	}

	for i, ch := range chs {
		for {
			select {
			case r := <-ch:
				glog.V(3).Infof("server %v returned result %#v for command %v",
					s.h, res, cmds[i])
				(*res)[i] = r
				return nil

			case <-time.After(10 * time.Second):
				glog.Errorf("%v is blocked on %v (chan %p size=%d)", s.h, cmds[i], ch,
					len(ch))
			}
		}
	}

	return nil
}

func (s *rpcServer) ProcessRaft(batch raft.Batch, dummy *bool) (err error) {
	if batch.To != s.h.ID() {
		glog.Fatalf("%v recieves a raft message for %v", s.h, msg.To)
	}

	glog.V(3).Infof("%v handles a batch from %v", s.h, batch.From)
	ctx, cnl := context.WithTimeout(context.Background(),
		s.h.config.RaftHBTimeout())
	err = s.h.node.StepBatch(ctx, batch, 2*s.h.config.RaftHBTimeout())
	cnl()
	return
}

func (s *rpcServer) EnqueMsg(msgs []msg, dummy *struct{}) error {
	for i := range msgs {
		s.h.enqueMsg(&msgs[i])
	}
	return nil
}
