package internet_test

import (
	"context"
	gonet "net"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/testing/servers/tcp"
	. "github.com/xtls/xray-core/transport/internet"
)

func raceSockopt() *SocketConfig {
	return &SocketConfig{HappyEyeballs: &HappyEyeballsConfig{MaxConcurrentTry: 2}}
}

func taggedContext(tag string) context.Context {
	return session.ContextWithOutbounds(context.Background(), []*session.Outbound{{Tag: tag}})
}

func listenLoopback6(t *testing.T) (gonet.Listener, net.Port) {
	t.Helper()
	listener, err := gonet.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return listener, net.Port(listener.Addr().(*gonet.TCPAddr).Port)
}

func TestRegisteredAddressesAreRacedAndTheReachableOneWins(t *testing.T) {
	listener, port := listenLoopback6(t)
	defer listener.Close()

	const tag = "proxy:race"
	SetDialAddresses(tag, []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("::1")})
	defer ClearDialAddresses(tag)

	ctx, cancel := context.WithTimeout(taggedContext(tag), 5*time.Second)
	defer cancel()
	started := time.Now()
	conn, err := DialSystem(ctx, net.TCPDestination(net.ParseAddress("192.0.2.1"), port), raceSockopt())
	common.Must(err)
	defer conn.Close()

	if got := conn.RemoteAddr().String(); got != "[::1]:"+port.String() {
		t.Fatalf("connected to %s, want the IPv6 listener", got)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("the race waited %v on the black-holed address instead of taking the one that answered", took)
	}
}

func TestRegisteredAddressesAreIgnoredWithoutHappyEyeballs(t *testing.T) {
	server := &tcp.Server{}
	dest, err := server.Start()
	common.Must(err)
	defer server.Close()

	const tag = "proxy:plain"
	SetDialAddresses(tag, []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("::1")})
	defer ClearDialAddresses(tag)

	conn, err := DialSystem(taggedContext(tag), net.TCPDestination(net.LocalHostIP, dest.Port), &SocketConfig{})
	common.Must(err)
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:"+dest.Port.String() {
		t.Fatalf("connected to %s, want the configured destination", got)
	}
}

func TestAnUnregisteredTagDialsTheDestination(t *testing.T) {
	server := &tcp.Server{}
	dest, err := server.Start()
	common.Must(err)
	defer server.Close()

	conn, err := DialSystem(taggedContext("proxy:none"), net.TCPDestination(net.LocalHostIP, dest.Port), raceSockopt())
	common.Must(err)
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:"+dest.Port.String() {
		t.Fatalf("connected to %s, want the configured destination", got)
	}
}

func TestClearedAddressesStopTheRace(t *testing.T) {
	server := &tcp.Server{}
	dest, err := server.Start()
	common.Must(err)
	defer server.Close()

	const tag = "proxy:cleared"
	SetDialAddresses(tag, []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("::1")})
	ClearDialAddresses(tag)

	conn, err := DialSystem(taggedContext(tag), net.TCPDestination(net.LocalHostIP, dest.Port), raceSockopt())
	common.Must(err)
	defer conn.Close()
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:"+dest.Port.String() {
		t.Fatalf("connected to %s, want the configured destination", got)
	}
}
