package internet

import (
	"context"
	"errors"
	gonet "net"
	"sync"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
)

type scriptedDialer struct {
	mu     sync.Mutex
	stall  map[string]bool
	refuse map[string]bool
	dialed []string
}

type addrConn struct {
	gonet.Conn
	remote gonet.Addr
}

func (c addrConn) RemoteAddr() gonet.Addr { return c.remote }

func (d *scriptedDialer) Dial(ctx context.Context, _ net.Address, dest net.Destination, _ *SocketConfig) (net.Conn, error) {
	ip := dest.Address.IP().String()
	d.mu.Lock()
	d.dialed = append(d.dialed, ip)
	d.mu.Unlock()
	switch {
	case d.stall[ip]:
		<-ctx.Done()
		return nil, ctx.Err()
	case d.refuse[ip]:
		return nil, errors.New("connection refused")
	}
	local, remote := gonet.Pipe()
	remote.Close()
	return addrConn{Conn: local, remote: &gonet.TCPAddr{IP: dest.Address.IP(), Port: int(dest.Port)}}, nil
}

func (d *scriptedDialer) DestIpAddress() net.IP { return nil }

func (d *scriptedDialer) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.dialed
	d.dialed = nil
	return out
}

func withScriptedDialer(t *testing.T, d *scriptedDialer) {
	t.Helper()
	saved := effectiveSystemDialer
	effectiveSystemDialer = d
	ResetRaceWins()
	t.Cleanup(func() {
		effectiveSystemDialer = saved
		ResetRaceWins()
	})
}

func raceThrough(t *testing.T, tag string, ips ...string) (string, time.Duration) {
	t.Helper()
	parsed := make([]net.IP, len(ips))
	for i, ip := range ips {
		parsed[i] = net.ParseIP(ip)
	}
	SetDialAddresses(tag, parsed)
	t.Cleanup(func() { ClearDialAddresses(tag) })
	ctx, cancel := context.WithTimeout(session.ContextWithOutbounds(context.Background(), []*session.Outbound{{Tag: tag}}), 5*time.Second)
	defer cancel()
	sockopt := &SocketConfig{HappyEyeballs: &HappyEyeballsConfig{MaxConcurrentTry: 2, TryDelayMs: 200}}
	started := time.Now()
	conn, err := DialSystem(ctx, net.TCPDestination(net.ParseAddress(ips[0]), 443), sockopt)
	if err != nil {
		t.Fatalf("DialSystem: %v", err)
	}
	defer conn.Close()
	return conn.RemoteAddr().(*gonet.TCPAddr).IP.String(), time.Since(started)
}

func TestAStaggeredRaceDialsOneAddressWhenItConnects(t *testing.T) {
	d := &scriptedDialer{}
	withScriptedDialer(t, d)

	won, _ := raceThrough(t, "proxy:one", "198.51.100.7", "2001:db8::7")
	if won != "198.51.100.7" {
		t.Fatalf("won by %s, want IPv4, dialled first before any race has been won", won)
	}
	time.Sleep(300 * time.Millisecond)
	if got := d.take(); len(got) != 1 {
		t.Fatalf("dialled %v, want only the first address", got)
	}
}

func TestAStaggeredRaceDialsTheOtherFamilyOnlyAfterTheDelayAndThenPrefersIt(t *testing.T) {
	d := &scriptedDialer{stall: map[string]bool{"198.51.100.7": true}}
	withScriptedDialer(t, d)

	won, took := raceThrough(t, "proxy:stall", "198.51.100.7", "2001:db8::7")
	if won != "2001:db8::7" || took < 200*time.Millisecond {
		t.Fatalf("won by %s after %v, want IPv6 once the 200 ms delay ran out", won, took)
	}
	if got := d.take(); len(got) != 2 || got[0] != "198.51.100.7" {
		t.Fatalf("dialled %v, want IPv4 then IPv6", got)
	}

	d.stall = nil
	won, took = raceThrough(t, "proxy:stall", "198.51.100.7", "2001:db8::7")
	if won != "2001:db8::7" || took > 100*time.Millisecond {
		t.Fatalf("won by %s after %v, want IPv6 at once, the family that won last", won, took)
	}
	time.Sleep(300 * time.Millisecond)
	if got := d.take(); len(got) != 1 || got[0] != "2001:db8::7" {
		t.Fatalf("dialled %v, want IPv6 alone", got)
	}
	if v4, v6 := RaceWins(); v4 != 0 || v6 != 2 {
		t.Fatalf("wins v4 %d v6 %d, want v6 2", v4, v6)
	}
}

func TestAStaggeredRaceMovesOnAtOnceWhenTheFirstAddressFails(t *testing.T) {
	d := &scriptedDialer{refuse: map[string]bool{"198.51.100.7": true}}
	withScriptedDialer(t, d)

	won, took := raceThrough(t, "proxy:refused", "198.51.100.7", "2001:db8::7")
	if won != "2001:db8::7" || took > 100*time.Millisecond {
		t.Fatalf("won by %s after %v, want IPv6 as soon as IPv4 was refused", won, took)
	}
}

func TestResetRaceWinsForgetsThePreferredFamily(t *testing.T) {
	d := &scriptedDialer{refuse: map[string]bool{"198.51.100.7": true}}
	withScriptedDialer(t, d)
	raceThrough(t, "proxy:reset", "198.51.100.7", "2001:db8::7")
	d.take()

	ResetRaceWins()
	d.refuse = nil
	if won, _ := raceThrough(t, "proxy:reset", "198.51.100.7", "2001:db8::7"); won != "198.51.100.7" {
		t.Fatalf("won by %s after a reset, want IPv4 first again", won)
	}
}
