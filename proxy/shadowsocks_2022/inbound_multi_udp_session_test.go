package shadowsocks_2022

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	gonet "net"
	"testing"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
)

type packetConn struct {
	gonet.Conn
	packets [][]byte
}

func (c *packetConn) Read(p []byte) (int, error) {
	if len(c.packets) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.packets[0])
	c.packets = c.packets[1:]
	return n, nil
}

func TestUnauthenticatedUDPLeavesNoSession(t *testing.T) {
	instance, err := core.New(&core.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), core.XrayKey(1), instance)
	ctx = session.ContextWithInbound(ctx, &session.Inbound{})

	serverKey := make([]byte, 16)
	userKey := make([]byte, 16)
	rand.Read(serverKey)
	rand.Read(userKey)
	inbound, err := NewMultiServer(ctx, &MultiUserServerConfig{
		Method: MethodAES128GCM,
		Key:    base64.StdEncoding.EncodeToString(serverKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbound.AddUser(ctx, &protocol.MemoryUser{Email: "user", Account: &MemoryAccount{Key: userKey}}); err != nil {
		t.Fatal(err)
	}

	var packets [][]byte
	for range 100 {
		junk := make([]byte, 64)
		rand.Read(junk)
		packets = append(packets, junk)
	}
	method, err := GetCipherMethod(MethodAES128GCM)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := NewUDPPacketCodec(method, [][]byte{serverKey, userKey})
	if err != nil {
		t.Fatal(err)
	}
	client, err := codec.NewClientSession()
	if err != nil {
		t.Fatal(err)
	}
	packet, err := client.EncodePacket(net.UDPDestination(net.LocalHostIP, 53), []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), packet.Bytes()...)
	tampered[len(tampered)-1] ^= 1
	packets = append(packets, tampered)

	inbound.Process(ctx, net.Network_UDP, &packetConn{packets: packets}, nil)

	sessions := 0
	inbound.udpSessions.sessions.Range(func(uint64, *ServerUDPSession) bool {
		sessions++
		return true
	})
	if sessions != 0 {
		t.Fatalf("%d sessions left behind by packets that never authenticated", sessions)
	}
}
