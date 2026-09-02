package tun

import (
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
)

// A one-shot UDP flow — a DNS query is the common case — used to sit in
// udpConns, with its handleConnection goroutine blocked reading conn.egress,
// until whatever downstream protocol handler happened to run its own idle
// timeout and call Close(). reapLoop closes it on its own; this pins that it
// actually does.
func TestReapClosesAnIdleFlow(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	handler := newUdpConnectionHandler(
		func(conn net.Conn, dest net.Destination) {
			defer wg.Done()
			buf := make([]byte, 1)
			// Mirrors proxy/dns.Handler.Process: one successful read for the
			// query, then a second read that blocks until something closes
			// the flow — here, the reaper rather than a downstream timeout.
			if _, err := conn.Read(buf); err != nil {
				t.Errorf("unexpected error reading the query itself: %v", err)
				return
			}
			if _, err := conn.Read(buf); err == nil {
				t.Error("expected the second read to fail once the flow is reaped")
			}
		},
		func(data []byte, src net.Destination, dst net.Destination) error { return nil },
		20*time.Millisecond,
	)
	defer handler.Close()

	src := net.UDPDestination(net.LocalHostIP, 12345)
	dst := net.UDPDestination(net.LocalHostIP, 53)
	handler.HandlePacket(src, dst, []byte{1})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("flow was not reaped in time")
	}

	handler.RLock()
	_, found := handler.udpConns[src]
	handler.RUnlock()
	if found {
		t.Fatal("reaped flow is still in udpConns")
	}
}

// A flow that keeps sending packets must survive across reap sweeps: aging
// is about idleness, not age.
func TestReapDoesNotCloseAnActiveFlow(t *testing.T) {
	handled := make(chan struct{})
	handler := newUdpConnectionHandler(
		func(conn net.Conn, dest net.Destination) {
			close(handled)
			buf := make([]byte, 1)
			conn.Read(buf) //nolint:errcheck // blocks until the test closes the handler
		},
		func(data []byte, src net.Destination, dst net.Destination) error { return nil },
		30*time.Millisecond,
	)
	defer handler.Close()

	src := net.UDPDestination(net.LocalHostIP, 12345)
	dst := net.UDPDestination(net.LocalHostIP, 53)
	handler.HandlePacket(src, dst, []byte{1})
	<-handled

	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		handler.HandlePacket(src, dst, []byte{2})
		time.Sleep(10 * time.Millisecond)
	}

	handler.RLock()
	_, found := handler.udpConns[src]
	handler.RUnlock()
	if !found {
		t.Fatal("an active flow was reaped")
	}
}

// Close has to drain, not just stop the reaper. Stopping it and walking away
// strands every still-open flow's goroutine on <-c.egress until the downstream
// handler's own idle timer gets to it — long after the disconnect that was
// meant to release them.
func TestCloseDrainsLiveFlows(t *testing.T) {
	var wg sync.WaitGroup
	handler := newUdpConnectionHandler(
		func(conn net.Conn, dest net.Destination) {
			defer wg.Done()
			buf := make([]byte, 1)
			for {
				if _, err := conn.Read(buf); err != nil {
					return
				}
			}
		},
		func(data []byte, src net.Destination, dst net.Destination) error { return nil },
		time.Hour, // long enough that only Close can end these flows
	)

	dst := net.UDPDestination(net.LocalHostIP, 53)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		handler.HandlePacket(net.UDPDestination(net.LocalHostIP, net.Port(30000+i)), dst, []byte{1})
	}

	handler.Close()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close left flow goroutines blocked")
	}

	handler.RLock()
	remaining := len(handler.udpConns)
	handler.RUnlock()
	if remaining != 0 {
		t.Fatalf("Close left %d flows in udpConns", remaining)
	}
}

// A reaped flow is closed twice: by reapExpired, and again by
// handleConnection's own defer once that close unblocks it. If the same
// source port has been reused in between, the second close must not evict the
// new flow that now owns the key.
func TestSecondCloseDoesNotEvictAReusedSource(t *testing.T) {
	handler := newUdpConnectionHandler(
		func(conn net.Conn, dest net.Destination) {},
		func(data []byte, src net.Destination, dst net.Destination) error { return nil },
		0, // no reaper: this drives both closes by hand
	)
	defer handler.Close()

	src := net.UDPDestination(net.LocalHostIP, 12345)
	dst := net.UDPDestination(net.LocalHostIP, 53)

	handler.HandlePacket(src, dst, []byte{1})
	handler.RLock()
	first := handler.udpConns[src]
	handler.RUnlock()

	first.Close() // the reaper's close

	handler.HandlePacket(src, dst, []byte{2}) // same port, new flow
	handler.RLock()
	second := handler.udpConns[src]
	handler.RUnlock()
	if second == nil || second == first {
		t.Fatal("expected a new conn to own the reused source port")
	}

	first.Close() // handleConnection's deferred close, arriving late

	handler.RLock()
	current, found := handler.udpConns[src]
	handler.RUnlock()
	if !found || current != second {
		t.Fatal("the late second close evicted the new flow")
	}
}

// Regression guard for the "send on closed channel" panic XTLS/Xray-core#5895
// and #5930 hit in production (fixed upstream by #5888, cherry-picked here):
// HandlePacket sending on conn.egress concurrently with reapLoop closing it.
// Meaningful under -race.
func TestHandlePacketDoesNotPanicRacingReap(t *testing.T) {
	handler := newUdpConnectionHandler(
		func(conn net.Conn, dest net.Destination) {
			buf := make([]byte, 1)
			for {
				if _, err := conn.Read(buf); err != nil {
					return
				}
			}
		},
		func(data []byte, src net.Destination, dst net.Destination) error { return nil },
		time.Millisecond,
	)
	defer handler.Close()

	dst := net.UDPDestination(net.LocalHostIP, 53)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		src := net.UDPDestination(net.LocalHostIP, net.Port(20000+i))
		go func(src net.Destination) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				handler.HandlePacket(src, dst, []byte{byte(j)})
			}
		}(src)
	}
	wg.Wait()
}

func TestHandlePacketDropsAFlowThatCameFromOurOwnSocket(t *testing.T) {
	handled := make(chan net.Destination, 2)
	handler := newUdpConnectionHandler(
		func(conn net.Conn, dest net.Destination) {
			handled <- net.DestinationFromAddr(conn.RemoteAddr())
		},
		func(data []byte, src net.Destination, dst net.Destination) error { return nil },
		time.Second,
	)
	defer handler.Close()
	handler.ownPort = func(port net.Port) bool { return port == 40000 }

	dst := net.UDPDestination(net.LocalHostIP, 123)
	looped := net.UDPDestination(net.LocalHostIP, 40000)
	foreign := net.UDPDestination(net.LocalHostIP, 40001)
	handler.HandlePacket(looped, dst, []byte{1})
	handler.HandlePacket(foreign, dst, []byte{1})

	select {
	case got := <-handled:
		if got != foreign {
			t.Fatalf("handled %v, expected only %v", got, foreign)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the flow from a foreign port was not handled")
	}

	handler.RLock()
	_, found := handler.udpConns[looped]
	flows := len(handler.udpConns)
	handler.RUnlock()
	if found || flows != 1 {
		t.Fatalf("looped flow present=%v, flows=%d", found, flows)
	}
	if n := handler.loopDrops.Load(); n != 1 {
		t.Fatalf("loopDrops = %d, expected 1", n)
	}
}

func TestHandlePacketDropsNewFlowsWhenTheTableIsFull(t *testing.T) {
	handler := newUdpConnectionHandler(
		func(conn net.Conn, dest net.Destination) {
			buf := make([]byte, 1)
			for {
				if _, err := conn.Read(buf); err != nil {
					return
				}
			}
		},
		func(data []byte, src net.Destination, dst net.Destination) error { return nil },
		time.Second,
	)
	defer handler.Close()

	dst := net.UDPDestination(net.LocalHostIP, 123)
	for i := 0; i < maxUDPFlows+10; i++ {
		handler.HandlePacket(net.UDPDestination(net.LocalHostIP, net.Port(1000+i)), dst, []byte{1})
	}

	handler.RLock()
	flows := len(handler.udpConns)
	handler.RUnlock()
	if flows != maxUDPFlows {
		t.Fatalf("flows = %d, expected %d", flows, maxUDPFlows)
	}
	if n := handler.fullDrops.Load(); n != 10 {
		t.Fatalf("fullDrops = %d, expected 10", n)
	}

	handler.HandlePacket(net.UDPDestination(net.LocalHostIP, 1000), dst, []byte{2})
	if n := handler.fullDrops.Load(); n != 10 {
		t.Fatalf("a packet on an existing flow was dropped: fullDrops = %d", n)
	}
}
