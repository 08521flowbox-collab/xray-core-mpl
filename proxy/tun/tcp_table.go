package tun

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
)

// tcpEntry is a connection that the stack handed to the handler, stamped by what is read from it and written to it
type tcpEntry struct {
	src, dst   net.Destination
	cancel     context.CancelFunc
	close      func() error
	lastActive atomic.Int64
}

func (e *tcpEntry) touch() {
	e.lastActive.Store(time.Now().UnixNano())
}

type tcpTable struct {
	max       int
	mu        sync.Mutex
	conns     map[*tcpEntry]struct{}
	evictions atomic.Uint64
}

func newTCPTable(max int) *tcpTable {
	return &tcpTable{max: max, conns: make(map[*tcpEntry]struct{})}
}

func (t *tcpTable) add(e *tcpEntry) {
	e.touch()
	t.mu.Lock()
	var quietest *tcpEntry
	if t.max > 0 && len(t.conns) >= t.max {
		for o := range t.conns {
			if quietest == nil || o.lastActive.Load() < quietest.lastActive.Load() {
				quietest = o
			}
		}
		delete(t.conns, quietest)
	}
	t.conns[e] = struct{}{}
	t.mu.Unlock()
	if quietest != nil {
		t.evict(quietest, e)
	}
}

func (t *tcpTable) remove(e *tcpEntry) {
	t.mu.Lock()
	delete(t.conns, e)
	t.mu.Unlock()
}

func (t *tcpTable) evict(e, newcomer *tcpEntry) {
	quiet := time.Duration(time.Now().UnixNano() - e.lastActive.Load())
	e.cancel()
	e.close()
	n := t.evictions.Add(1)
	if n == 1 || n%1000 == 0 {
		errors.LogWarning(context.Background(), "closed the tcp connection quiet longest, the tun connection table is full: it went to ", e.dst.NetAddr(),
			" and had been quiet ", quiet.Truncate(time.Millisecond), "; new connection to ", newcomer.dst.NetAddr(), ", evicted ", n)
	}
}

func (t *tcpTable) census(top int) (int, []Share) {
	t.mu.Lock()
	live := len(t.conns)
	byDst := make(map[net.Destination]*Share)
	if top > 0 {
		for e := range t.conns {
			s := byDst[e.dst]
			if s == nil {
				s = &Share{Dst: e.dst, Src: e.src}
				byDst[e.dst] = s
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
