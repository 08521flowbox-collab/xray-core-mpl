package tun

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

type tcpConn struct {
	*gonet.TCPConn
	src, dst   net.Destination
	cancel     context.CancelFunc
	lastActive atomic.Int64
}

func (c *tcpConn) Read(b []byte) (int, error) {
	n, err := c.TCPConn.Read(b)
	if n > 0 {
		c.lastActive.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *tcpConn) Write(b []byte) (int, error) {
	n, err := c.TCPConn.Write(b)
	if n > 0 {
		c.lastActive.Store(time.Now().UnixNano())
	}
	return n, err
}

type tcpTable struct {
	max       int
	mu        sync.Mutex
	conns     map[*tcpConn]struct{}
	evictions atomic.Uint64
}

func newTCPTable(max int) *tcpTable {
	return &tcpTable{max: max, conns: make(map[*tcpConn]struct{})}
}

func (t *tcpTable) add(c *tcpConn) {
	c.lastActive.Store(time.Now().UnixNano())
	t.mu.Lock()
	var quietest *tcpConn
	if t.max > 0 && len(t.conns) >= t.max {
		for o := range t.conns {
			if quietest == nil || o.lastActive.Load() < quietest.lastActive.Load() {
				quietest = o
			}
		}
		delete(t.conns, quietest)
	}
	t.conns[c] = struct{}{}
	t.mu.Unlock()
	if quietest != nil {
		t.evict(quietest, c)
	}
}

func (t *tcpTable) remove(c *tcpConn) {
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
}

func (t *tcpTable) evict(c, newcomer *tcpConn) {
	quiet := time.Duration(time.Now().UnixNano() - c.lastActive.Load())
	c.cancel()
	c.TCPConn.Close()
	n := t.evictions.Add(1)
	if n == 1 || n%1000 == 0 {
		errors.LogWarning(context.Background(), "closed the tcp connection quiet longest, the tun connection table is full: it went to ", c.dst.NetAddr(),
			" and had been quiet ", quiet.Truncate(time.Millisecond), "; new connection to ", newcomer.dst.NetAddr(), ", evicted ", n)
	}
}

func (t *tcpTable) census(top int) (int, []Share) {
	t.mu.Lock()
	live := len(t.conns)
	byDst := make(map[net.Destination]*Share)
	if top > 0 {
		for c := range t.conns {
			s := byDst[c.dst]
			if s == nil {
				s = &Share{Dst: c.dst, Src: c.src}
				byDst[c.dst] = s
			}
			s.Count++
		}
	}
	t.mu.Unlock()
	shares := make([]Share, 0, len(byDst))
	for _, s := range byDst {
		shares = append(shares, *s)
	}
	sort.Slice(shares, func(i, j int) bool { return shares[i].Count > shares[j].Count })
	if len(shares) > top {
		shares = shares[:top]
	}
	return live, shares
}

type Census struct {
	TCP, UDP               int
	TCPEvicted, UDPEvicted uint64
	Top                    []Share
}

type Share struct {
	Dst, Src net.Destination
	Count    int
}
