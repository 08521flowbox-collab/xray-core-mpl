package internet

import (
	"context"
	gonet "net"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
)

func TestFullSlotsEvictTheOldestDial(t *testing.T) {
	SetMaxConcurrentSystemDials(2)
	defer SetMaxConcurrentSystemDials(0)

	oldestCtx, releaseOldest, err := acquireSystemDial(context.Background(), "192.0.2.1:443")
	if err != nil {
		t.Fatalf("first dial slot: %v", err)
	}
	youngerCtx, releaseYounger, err := acquireSystemDial(context.Background(), "192.0.2.2:443")
	if err != nil {
		t.Fatalf("second dial slot: %v", err)
	}

	start := time.Now()
	_, releaseNewcomer, err := acquireSystemDial(context.Background(), "192.0.2.3:443")
	if err != nil {
		t.Fatalf("third dial slot: %v", err)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("the new dial waited %v instead of evicting the oldest", took)
	}
	if oldestCtx.Err() != context.Canceled {
		t.Fatalf("the oldest dial saw %v, want context.Canceled", oldestCtx.Err())
	}
	if youngerCtx.Err() != nil {
		t.Fatalf("the younger dial was cancelled too: %v", youngerCtx.Err())
	}

	releaseOldest()
	releaseYounger()
	releaseNewcomer()

	start = time.Now()
	_, release, err := acquireSystemDial(context.Background(), "192.0.2.4:443")
	if err != nil {
		t.Fatalf("dial slot after the evictions: %v", err)
	}
	release()
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("the slots were not accounted for after an eviction: waited %v", took)
	}
}

func TestEvictionKeepsTheSlotCountStable(t *testing.T) {
	SetMaxConcurrentSystemDials(1)
	defer SetMaxConcurrentSystemDials(0)

	releases := make([]func(), 0, 20)
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	for i := 0; i < 20; i++ {
		start := time.Now()
		_, release, err := acquireSystemDial(context.Background(), "192.0.2.9:443")
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		if took := time.Since(start); took > 100*time.Millisecond {
			t.Fatalf("dial %d waited %v; a slot went missing", i, took)
		}
		releases = append(releases, release)
	}
}

func TestResetSystemDialsUnblocksAConnectingDial(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	addr := listener.Addr().(*net.TCPAddr)

	var held []net.Conn
	defer func() {
		for _, conn := range held {
			conn.Close()
		}
	}()
	full := false
	for i := 0; i < 512 && !full; i++ {
		conn, err := gonet.DialTimeout("tcp", addr.String(), 300*time.Millisecond)
		if err != nil {
			full = true
			break
		}
		held = append(held, conn)
	}
	if !full {
		t.Skipf("the accept queue never filled after %d connections; nothing blocks in connect()", len(held))
	}

	dialer := &DefaultSystemDialer{}
	dest := net.TCPDestination(net.ParseAddress("127.0.0.1"), net.Port(addr.Port))

	done := make(chan error, 1)
	go func() {
		conn, err := dialer.Dial(context.Background(), nil, dest, nil)
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Skipf("the dial did not block on a full accept queue (%v)", err)
	case <-time.After(500 * time.Millisecond):
	}

	start := time.Now()
	if inFlight := ResetSystemDials(); inFlight < 1 {
		t.Fatalf("reset saw %d dials in flight, want at least 1", inFlight)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the blackhole dial reported success")
		}
		t.Logf("dial returned %v, %v after the reset", err, time.Since(start))
	case <-time.After(2 * time.Second):
		t.Fatal("reset did not unblock the dial")
	}
}
