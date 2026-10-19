package tun

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

type holdingDispatcher struct {
	accepted chan net.Port
	read     chan net.Port
	released chan net.Port
}

func newHoldingDispatcher() *holdingDispatcher {
	return &holdingDispatcher{
		accepted: make(chan net.Port, 16),
		read:     make(chan net.Port, 16),
		released: make(chan net.Port, 16),
	}
}

func (*holdingDispatcher) Type() interface{} { return routing.DispatcherType() }
func (*holdingDispatcher) Start() error      { return nil }
func (*holdingDispatcher) Close() error      { return nil }

func (*holdingDispatcher) Dispatch(context.Context, net.Destination) (*transport.Link, error) {
	return nil, errors.New("not used")
}

func (d *holdingDispatcher) DispatchLink(ctx context.Context, _ net.Destination, link *transport.Link) error {
	port := session.InboundFromContext(ctx).Source.Port
	go func() {
		for {
			mb, err := link.Reader.ReadMultiBuffer()
			if !mb.IsEmpty() {
				d.read <- port
			}
			buf.ReleaseMulti(mb)
			if err != nil {
				return
			}
		}
	}()
	d.accepted <- port
	<-ctx.Done()
	d.released <- port
	return nil
}

func connectThroughTun(t *testing.T, client *stack.Stack, addr [4]byte, port uint16) (tcpip.Endpoint, net.Port) {
	t.Helper()
	var wq waiter.Queue
	ep, err := client.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ep.Close)
	entry, ready := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)
	err = ep.Connect(tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(addr), Port: port})
	if _, started := err.(*tcpip.ErrConnectStarted); started {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("connect through the tun stack timed out")
		}
		err = ep.LastError()
	}
	if err != nil {
		t.Fatal(err)
	}
	local, err := ep.GetLocalAddress()
	if err != nil {
		t.Fatal(err)
	}
	return ep, net.Port(local.Port)
}

func expectPort(t *testing.T, ch chan net.Port, want net.Port, what string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("%s: got the connection from port %d, want %d", what, got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nothing from port %d", what, want)
	}
}

func TestFullTableClosesTheConnectionQuietLongestNotTheOldest(t *testing.T) {
	t.Setenv(platform.TunMaxTCPConnsKey, "2")
	d := newHoldingDispatcher()
	s, client := startTestStack(t, d)

	talker, talkerPort := connectThroughTun(t, client, [4]byte{1, 2, 3, 4}, 443)
	expectPort(t, d.accepted, talkerPort, "first connection")
	_, quietPort := connectThroughTun(t, client, [4]byte{1, 2, 3, 4}, 443)
	expectPort(t, d.accepted, quietPort, "second connection")

	if _, err := talker.Write(bytes.NewReader([]byte("ping")), tcpip.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	expectPort(t, d.read, talkerPort, "data from the first connection")

	_, newPort := connectThroughTun(t, client, [4]byte{1, 2, 3, 4}, 443)
	expectPort(t, d.accepted, newPort, "third connection")
	expectPort(t, d.released, quietPort, "eviction")
	select {
	case port := <-d.released:
		t.Fatalf("the connection from port %d was also closed; only the quiet one should be", port)
	case <-time.After(200 * time.Millisecond):
	}

	c := s.census(0)
	if c.TCP != 2 || c.TCPEvicted != 1 {
		t.Fatalf("census = %d open, %d evicted; want 2 and 1", c.TCP, c.TCPEvicted)
	}
}

func TestUnboundedTableNeverEvicts(t *testing.T) {
	d := newHoldingDispatcher()
	s, client := startTestStack(t, d)
	for i := 0; i < 3; i++ {
		_, port := connectThroughTun(t, client, [4]byte{1, 2, 3, 4}, 443)
		expectPort(t, d.accepted, port, "connection")
	}
	if c := s.census(0); c.TCP != 3 || c.TCPEvicted != 0 {
		t.Fatalf("census = %d open, %d evicted; want 3 and 0", c.TCP, c.TCPEvicted)
	}
}

func TestCensusRanksDestinationsAndKeepsASource(t *testing.T) {
	d := newHoldingDispatcher()
	s, client := startTestStack(t, d)
	var busyPort net.Port
	for i := 0; i < 2; i++ {
		_, busyPort = connectThroughTun(t, client, [4]byte{1, 2, 3, 4}, 443)
		expectPort(t, d.accepted, busyPort, "busy destination")
	}
	_, port := connectThroughTun(t, client, [4]byte{5, 6, 7, 8}, 80)
	expectPort(t, d.accepted, port, "other destination")

	c := s.census(1)
	if c.TCP != 3 || len(c.Top) != 1 {
		t.Fatalf("census = %d open with %d top destinations; want 3 and 1", c.TCP, len(c.Top))
	}
	top := c.Top[0]
	if top.Dst.NetAddr() != "1.2.3.4:443" || top.Count != 2 {
		t.Fatalf("top destination = %s with %d; want 1.2.3.4:443 with 2", top.Dst.NetAddr(), top.Count)
	}
	if top.Src.Address.String() != "10.0.0.2" || top.Src.Port == 0 {
		t.Fatalf("top destination's source = %s; want one of the client's connections from 10.0.0.2", top.Src.NetAddr())
	}
}

func TestEndedConnectionLeavesTheTable(t *testing.T) {
	d := &answeringDispatcher{accepted: make(chan struct{}, 1)}
	s, client := startTestStack(t, d)
	dialThroughTun(t, client, 0)
	awaitAccepted(t, d)
	deadline := time.Now().Add(5 * time.Second)
	for s.census(0).TCP != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a connection whose handler returned is still in the table")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
