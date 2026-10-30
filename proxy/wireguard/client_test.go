package wireguard

import (
	gonet "net"
	"testing"

	"github.com/xtls/xray-core/common/net"
)

func TestUDPConnClientLabelsRepliesFromTheTargetWithTheTarget(t *testing.T) {
	local, err := gonet.ListenPacket("udp", "[::]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	port := local.LocalAddr().(*gonet.UDPAddr).Port

	target, err := gonet.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	other, err := gonet.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	c := &UDPConnClient{
		PacketConn: local,
		Dest:       target.LocalAddr().(*gonet.UDPAddr),
		Target:     net.DomainAddress("stun.example.com"),
	}

	if _, err := target.WriteTo([]byte("a"), &gonet.UDPAddr{IP: gonet.IPv4(127, 0, 0, 1), Port: port}); err != nil {
		t.Fatal(err)
	}
	mb, err := c.ReadMultiBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if got := mb[0].UDP; got.Address != c.Target || got.Port != net.Port(c.Dest.Port) {
		t.Fatalf("reply from the target labelled %v, want %v:%d", got, c.Target, c.Dest.Port)
	}

	if _, err := other.WriteTo([]byte("b"), &gonet.UDPAddr{IP: gonet.IPv6loopback, Port: port}); err != nil {
		t.Fatal(err)
	}
	mb, err = c.ReadMultiBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if got := mb[0].UDP.Address; !got.Family().IsIP() || !got.IP().Equal(gonet.IPv6loopback) {
		t.Fatalf("reply from another host labelled %v, want ::1", got)
	}
}
