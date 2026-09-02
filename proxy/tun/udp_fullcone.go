package tun

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
)

const maxUDPFlows = 4096

// portIdle is the idle timeout for flows whose destination port names a
// request/response protocol, the same table sing-box keeps as
// ProtocolTimeouts. A DNS or NTP exchange is one packet each way; keeping the
// flow for the general ConnectionIdle (300 s) left a memory-capped host
// holding hundreds of finished queries, three goroutines and an egress queue
// each. Only ever shortens the general timeout. A var so tests can shrink it.
var portIdle = map[net.Port]time.Duration{
	53:   10 * time.Second,
	123:  10 * time.Second,
	3478: 10 * time.Second,
	443:  30 * time.Second,
}

// egressQueue is how many packets a flow may have waiting for its handler.
// sing's NAT uses 64; the 1024 upstream chose is 8 KiB of pointers per flow.
const egressQueue = 64

type packet struct {
	data []byte
	dest *net.Destination
}

// sub-handler specifically for udp connections under main handler
type udpConnectionHandler struct {
	sync.RWMutex

	udpConns map[net.Destination]*udpConn

	handleConnection func(conn net.Conn, dest net.Destination)
	writePacket      func(data []byte, src net.Destination, dst net.Destination) error

	// idleTimeout paces reapLoop, which is this table's own aging. Without it,
	// a flow that never gets another packet — a one-shot DNS query is the
	// common case — sits in udpConns, and its handleConnection goroutine sits
	// blocked reading conn.egress, until whatever downstream handler happens
	// to run its own idle timeout and call Close(). See MODIFICATIONS.md.
	idleTimeout   time.Duration
	stopReap      chan struct{}
	closeReapOnce sync.Once

	// hijackDNS, when set, answers a query to port 53 without a flow. It runs
	// after ownPort, and that order is load-bearing: the DNS client's own
	// upstream query, were it ever to loop back through the tun, would
	// otherwise be answered by asking the DNS client again, forever.
	hijackDNS func(src, dst net.Destination, data []byte) bool

	ownPort   func(net.Port) bool
	loopDrops atomic.Uint64
	evictions atomic.Uint64
}

func newUdpConnectionHandler(handleConnection func(conn net.Conn, dest net.Destination), writePacket func(data []byte, src net.Destination, dst net.Destination) error, idleTimeout time.Duration) *udpConnectionHandler {
	handler := &udpConnectionHandler{
		udpConns:         make(map[net.Destination]*udpConn),
		handleConnection: handleConnection,
		writePacket:      writePacket,
		idleTimeout:      idleTimeout,
		stopReap:         make(chan struct{}),
		ownPort:          internet.IsOwnUDPPort,
	}

	if idleTimeout > 0 {
		go handler.reapLoop()
	}

	return handler
}

// HandlePacket handles UDP packets coming from tun, to forward to the dispatcher
// this custom handler support FullCone NAT of returning packets, binding connection only by the source addr:port
func (u *udpConnectionHandler) HandlePacket(src net.Destination, dst net.Destination, data []byte) {
	u.RLock()
	conn, found := u.udpConns[src]
	if found {
		conn.lastActive.Store(time.Now().UnixNano())
		select {
		case conn.egress <- &packet{
			data: data,
			dest: &dst,
		}:
		default:
			errors.LogDebug(context.Background(), "drop udp with size ", len(data), " to ", dst.NetAddr(), " original ", conn.dst.NetAddr(), " > queue full")
		}
		u.RUnlock()
		return
	}
	u.RUnlock()

	if u.ownPort(src.Port) {
		u.dropFlow(&u.loopDrops, "dropped a udp packet that came back into the tun from our own socket", src, dst)
		return
	}
	if dst.Port == 53 && u.hijackDNS != nil && u.hijackDNS(src, dst, data) {
		return
	}

	u.Lock()
	defer u.Unlock()

	conn, found = u.udpConns[src]
	if !found {
		if len(u.udpConns) >= maxUDPFlows {
			u.evictOldestLocked(src, dst)
		}
		egress := make(chan *packet, egressQueue)
		conn = &udpConn{handler: u, egress: egress, src: src, dst: dst}
		u.udpConns[src] = conn

		go u.handleConnection(conn, dst)
	}
	conn.lastActive.Store(time.Now().UnixNano())

	// send packet data to the egress channel, if it has buffer, or discard
	select {
	case conn.egress <- &packet{
		data: data,
		dest: &dst,
	}:
	default:
		errors.LogDebug(context.Background(), "drop udp with size ", len(data), " to ", dst.NetAddr(), " original ", conn.dst.NetAddr(), " > queue full")
	}
}

func (u *udpConnectionHandler) dropFlow(counter *atomic.Uint64, why string, src, dst net.Destination) {
	n := counter.Add(1)
	if n == 1 || n%1000 == 0 {
		errors.LogWarning(context.Background(), why, ": from ", src.NetAddr(), " to ", dst.NetAddr(), ", flows ", len(u.udpConns), ", dropped ", n)
	}
}

// evictOldestLocked makes room for a new flow by closing the one that has
// gone longest without a packet — an LRU, the way sing's NAT cache is —
// rather than refusing the newcomer. Caller holds the write lock.
func (u *udpConnectionHandler) evictOldestLocked(src, dst net.Destination) {
	var oldestSrc net.Destination
	var oldest *udpConn
	for s, c := range u.udpConns {
		if oldest == nil || c.lastActive.Load() < oldest.lastActive.Load() {
			oldestSrc, oldest = s, c
		}
	}
	if oldest == nil {
		return
	}
	delete(u.udpConns, oldestSrc)
	close(oldest.egress)
	n := u.evictions.Add(1)
	if n == 1 || n%1000 == 0 {
		errors.LogWarning(context.Background(), "evicted the oldest udp flow, the tun flow table is full: new flow from ", src.NetAddr(), " to ", dst.NetAddr(), ", evicted ", n)
	}
}

// connectionFinished takes the conn and not just its key, and that identity
// check is load-bearing here in a way it was not upstream. A reaped flow is
// closed twice — once by reapExpired, then again by handleConnection's own
// defer once the close unblocks it — and by the time the second call lands,
// the same source port may already have been reused by a new flow. Keyed only
// by src, that second call would evict a live conn that had just started.
func (u *udpConnectionHandler) connectionFinished(src net.Destination, conn *udpConn) {
	u.Lock()
	if current, found := u.udpConns[src]; found && current == conn {
		delete(u.udpConns, src)
		close(conn.egress)
	}
	u.Unlock()
}

// reapLoop closes any flow idleTimeout has not seen a packet for. It is the
// only cleanup path that does not depend on a downstream protocol handler
// noticing a flow went idle.
func (u *udpConnectionHandler) reapLoop() {
	shortest := u.idleTimeout
	for _, d := range portIdle {
		shortest = min(shortest, d)
	}
	interval := shortest / 4
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			u.reapExpired()
		case <-u.stopReap:
			return
		}
	}
}

func (u *udpConnectionHandler) reapExpired() {
	now := time.Now().UnixNano()
	u.evict(func(conn *udpConn) bool {
		idle := u.idleTimeout
		if d, ok := portIdle[conn.dst.Port]; ok && d < idle {
			idle = d
		}
		return conn.lastActive.Load() < now-int64(idle)
	})
}

// Close stops reapLoop and drains what is left. Draining is the half that is
// easy to leave out and expensive to leave out: stopping the reaper without it
// would strand every still-open flow's goroutine on `<-c.egress` until the
// downstream handler's own idle timer got to it, which for proxy/dns is two
// ConnectionIdle ticks — around ten minutes of blocked goroutines and 9 KiB
// egress buffers per flow, surviving the disconnect that was supposed to
// release them, and stacking with the next session's if the user reconnects
// inside the window.
//
// Idempotent: Handler.Close has more than one entrance.
func (u *udpConnectionHandler) Close() {
	u.closeReapOnce.Do(func() {
		close(u.stopReap)
	})
	u.evict(func(*udpConn) bool { return true })
}

// evict deletes and closes every flow matching keep, under one write lock.
//
// Inline rather than through conn.Close(): that would re-enter this same lock
// through connectionFinished, and a sync.RWMutex is not reentrant. Doing it
// here is also what removes a check-then-act window — deciding staleness under
// a read lock and closing after releasing it let a packet arrive in between,
// be accepted onto a flow's egress, and have that flow torn down anyway.
// Deleting during range is legal, and close never blocks, so the whole sweep
// is safe to hold the lock across.
//
// handleConnection's own deferred conn.Close() still runs afterwards; the
// identity check in connectionFinished absorbs it.
func (u *udpConnectionHandler) evict(stale func(*udpConn) bool) {
	u.Lock()
	for src, conn := range u.udpConns {
		if stale(conn) {
			delete(u.udpConns, src)
			close(conn.egress)
		}
	}
	u.Unlock()
}

// udp connection abstraction
type udpConn struct {
	handler *udpConnectionHandler

	egress     chan *packet
	src        net.Destination
	dst        net.Destination
	lastActive atomic.Int64
}

func (c *udpConn) ReadMultiBuffer() (buf.MultiBuffer, error) {
	for {
		e, ok := <-c.egress
		if !ok {
			return nil, io.EOF
		}

		b := buf.New()

		_, err := b.Write(e.data)
		if err != nil {
			errors.LogDebugInner(context.Background(), err, "drop udp with size ", len(e.data), " to ", e.dest.NetAddr(), " original ", c.dst.NetAddr())
			b.Release()
			continue
		}

		b.UDP = e.dest

		return buf.MultiBuffer{b}, nil
	}
}

// Read packets from the connection
func (c *udpConn) Read(p []byte) (int, error) {
	e, ok := <-c.egress
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, e.data)
	if n != len(e.data) {
		return 0, io.ErrShortBuffer
	}
	return n, nil
}

func (c *udpConn) WriteMultiBuffer(mb buf.MultiBuffer) error {
	for i, b := range mb {
		dst := c.dst
		if b.UDP != nil {
			if b.UDP.Address.Family().IsDomain() {
				errors.LogError(context.Background(), "impossible domain packet ", b.UDP, " reply via original target ", dst)
			} else {
				dst = *b.UDP
			}
		}
		err := c.handler.writePacket(b.Bytes(), dst, c.src)
		if err != nil {
			buf.ReleaseMulti(mb[i:])
			return err
		}
		b.Release()
	}
	return nil
}

// Write returning packets back
func (c *udpConn) Write(p []byte) (int, error) {
	// sending packets back mean sending payload with source/destination reversed
	err := c.handler.writePacket(p, c.dst, c.src)
	if err != nil {
		return 0, nil
	}

	return len(p), nil
}

func (c *udpConn) Close() error {
	c.handler.connectionFinished(c.src, c)

	return nil
}

func (c *udpConn) LocalAddr() net.Addr {
	return c.dst.RawNetAddr()
}

func (c *udpConn) RemoteAddr() net.Addr {
	return c.src.RawNetAddr()
}

func (c *udpConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *udpConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *udpConn) SetWriteDeadline(t time.Time) error {
	return nil
}
