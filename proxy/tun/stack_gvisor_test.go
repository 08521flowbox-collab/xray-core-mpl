//go:build !android

package tun

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

type channelTun struct{ ep *channel.Endpoint }

func (c channelTun) newEndpoint() (stack.LinkEndpoint, error) { return c.ep, nil }

type answeringDispatcher struct {
	payload  int
	hold     chan struct{}
	accepted chan struct{}
}

func (*answeringDispatcher) Type() interface{} { return routing.DispatcherType() }
func (*answeringDispatcher) Start() error      { return nil }
func (*answeringDispatcher) Close() error      { return nil }

func (*answeringDispatcher) Dispatch(context.Context, net.Destination) (*transport.Link, error) {
	return nil, errors.New("not used")
}

func (d *answeringDispatcher) DispatchLink(_ context.Context, _ net.Destination, link *transport.Link) error {
	if d.payload > 0 {
		if err := link.Writer.WriteMultiBuffer(buf.MergeBytes(nil, make([]byte, d.payload))); err != nil {
			return err
		}
	}
	d.accepted <- struct{}{}
	if d.hold != nil {
		<-d.hold
	}
	return nil
}

func relay(ctx context.Context, from, to *channel.Endpoint) {
	for {
		pkt := from.ReadContext(ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		pkt.DecRef()
		out := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithView(view)})
		to.InjectInbound(header.IPv4ProtocolNumber, out)
		out.DecRef()
	}
}

func startTestStack(t *testing.T, d routing.Dispatcher) (*stackGVisor, *stack.Stack) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tunEP := channel.New(512, 1500, "")
	clientEP := channel.New(512, 1500, "")
	s := &stackGVisor{
		ctx:     ctx,
		tun:     channelTun{tunEP},
		handler: &Handler{ctx: ctx, config: &Config{}, dispatcher: d, tag: "tun"},
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}

	client := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	if err := client.CreateNIC(1, clientEP); err != nil {
		t.Fatal(err)
	}
	if err := client.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4([4]byte{10, 0, 0, 2}).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	client.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})

	go relay(ctx, tunEP, clientEP)
	go relay(ctx, clientEP, tunEP)
	t.Cleanup(func() {
		cancel()
		client.Close()
		s.Close()
	})
	return s, client
}

func dialThroughTun(t *testing.T, client *stack.Stack, rcvBuf int64) {
	t.Helper()
	var wq waiter.Queue
	ep, err := client.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ep.Close)
	if rcvBuf > 0 {
		ep.SocketOptions().SetReceiveBufferSize(rcvBuf, true)
	}
	entry, ready := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)
	err = ep.Connect(tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{1, 2, 3, 4}), Port: 443})
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
}

func awaitAccepted(t *testing.T, d *answeringDispatcher) {
	t.Helper()
	select {
	case <-d.accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the tun stack never handed the connection to the dispatcher")
	}
}

func TestForwardedConnectionStartsAtTheConfiguredReceiveBuffer(t *testing.T) {
	t.Setenv(platform.TunTCPBufDefaultKey, "131072")
	t.Setenv(platform.TunTCPBufMaxKey, "524288")
	d := &answeringDispatcher{accepted: make(chan struct{}, 1), hold: make(chan struct{})}
	s, client := startTestStack(t, d)
	t.Cleanup(func() { close(d.hold) })
	dialThroughTun(t, client, 0)
	awaitAccepted(t, d)

	for _, registered := range s.stack.RegisteredEndpoints() {
		ep, ok := registered.(*tcp.Endpoint)
		if !ok {
			continue
		}
		if got := ep.SocketOptions().GetReceiveBufferSize(); got != 131072 {
			t.Fatalf("forwarded endpoint receive buffer = %d, want the configured 131072", got)
		}
		return
	}
	t.Fatal("no forwarded TCP endpoint registered")
}

func TestClosedConnectionStopsWaitingForAReaderThatStopped(t *testing.T) {
	saved := closedUnsentTimeout
	closedUnsentTimeout = time.Second
	t.Cleanup(func() { closedUnsentTimeout = saved })
	d := &answeringDispatcher{accepted: make(chan struct{}, 1), payload: 256 << 10}
	s, client := startTestStack(t, d)
	dialThroughTun(t, client, 16<<10)
	awaitAccepted(t, d)

	timedOut := s.stack.Stats().TCP.EstablishedTimedout
	deadline := time.Now().Add(10 * time.Second)
	for timedOut.Value() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a closed connection whose reader stopped still holds its unsent bytes")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
