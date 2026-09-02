package internet

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
)

func dialUDPLoopback(t *testing.T, network string, ip net.IP) (net.PacketConn, net.Conn) {
	t.Helper()
	server, err := net.ListenUDP(network, &net.UDPAddr{IP: ip})
	if err != nil {
		t.Skipf("%s loopback unavailable: %v", network, err)
	}
	t.Cleanup(func() { server.Close() })

	conn, err := (&DefaultSystemDialer{}).Dial(context.Background(), nil, net.DestinationFromAddr(server.LocalAddr()), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	server.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if _, _, err := server.ReadFrom(buf); err != nil {
		t.Fatalf("the packet never reached the server: %v", err)
	}
	return server, conn
}

func TestDialUDPBuildsAnIPv4SocketForAnIPv4Destination(t *testing.T) {
	_, conn := dialUDPLoopback(t, "udp4", net.IP{127, 0, 0, 1})
	defer conn.Close()

	local := conn.LocalAddr().(*net.UDPAddr)
	if local.IP.To4() == nil {
		t.Fatalf("expected an AF_INET socket, got local address %s", local)
	}
}

func TestDialUDPBuildsAnIPv6SocketForAnIPv6Destination(t *testing.T) {
	_, conn := dialUDPLoopback(t, "udp6", net.LocalHostIPv6.IP())
	defer conn.Close()

	local := conn.LocalAddr().(*net.UDPAddr)
	if local.IP.To4() != nil {
		t.Fatalf("expected an AF_INET6 socket, got local address %s", local)
	}
}

func TestDialUDPRemembersItsOwnPortUntilClosed(t *testing.T) {
	_, conn := dialUDPLoopback(t, "udp4", net.IP{127, 0, 0, 1})

	port := net.Port(conn.LocalAddr().(*net.UDPAddr).Port)
	if !IsOwnUDPPort(port) {
		t.Fatalf("port %d is not registered while the socket is open", port)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if IsOwnUDPPort(port) {
		t.Fatalf("port %d is still registered after close", port)
	}
}
