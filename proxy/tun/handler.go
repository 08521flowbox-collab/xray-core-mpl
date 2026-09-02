package tun

import (
	"context"
	"sync"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	c "github.com/xtls/xray-core/common/ctx"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet/stat"
)

// Handler is managing object that tie together tun interface, ip stack and dispatch connections to the routing
type Handler struct {
	ctx             context.Context
	config          *Config
	closeOnce       sync.Mutex
	tun             Tun
	stack           Stack
	policyManager   policy.Manager
	dispatcher      routing.Dispatcher
	dnsClient       dns.Client
	tag             string
	sniffingRequest session.SniffingRequest
}

// ConnectionHandler interface with the only method that stack is going to push new connections to
type ConnectionHandler interface {
	HandleConnection(conn net.Conn, destination net.Destination)
}

// Handler implements ConnectionHandler
var _ ConnectionHandler = (*Handler)(nil)

// Handler implements common.Closable
var _ common.Closable = (*Handler)(nil)

// Handler implements common.Runnable
var _ common.Runnable = (*Handler)(nil)

func (t *Handler) policy() policy.Session {
	p := t.policyManager.ForLevel(t.config.UserLevel)
	return p
}

// Init the Handler instance with necessary parameters.
//
// **It no longer brings the interface up**, and that is the whole of this
// split. Init runs while the instance is still being assembled, before the
// features have been started — and a tun that is already reading is already
// dispatching, so the first packet reached a FakeDNS holder whose ipRange had
// not been assigned yet and took the process down in
// `net.(*IPNet).Contains`. Upstream #6442 reports exactly that and was closed
// with "这是TUN入站启动过早的问题 已经修复TUN入站": the fix is the ordering,
// not a nil guard on the holder. See [Handler.Start] and MODIFICATIONS.md.
func (t *Handler) Init(ctx context.Context, pm policy.Manager, dispatcher routing.Dispatcher, dnsClient dns.Client) error {
	// Retrieve tag and sniffing config from context (set by AlwaysOnInboundHandler)
	if inbound := session.InboundFromContext(ctx); inbound != nil {
		t.tag = inbound.Tag
	}
	if content := session.ContentFromContext(ctx); content != nil {
		t.sniffingRequest = content.SniffingRequest
	}

	t.ctx = core.ToBackgroundDetachedContext(ctx)
	t.policyManager = pm
	t.dispatcher = dispatcher
	t.dnsClient = dnsClient

	return nil
}

// dnsHijack hands the stack a port-53 answerer bound to the DNS client, or
// nil when there is none to bind. See dns_fastpath.go.
func (t *Handler) dnsHijack(write func(payload []byte, src, dst net.Destination) error) func(src, dst net.Destination, data []byte) bool {
	if t.dnsClient == nil {
		return nil
	}
	fp := &dnsFastPath{client: t.dnsClient, write: write}
	return fp.Handle
}

// Start implements common.Runnable, and it is where the interface comes up.
//
// Reached from AlwaysOnInboundHandler.Start, which runs after core.Start has
// started every feature. A tun inbound has no listening port and therefore no
// worker, and a worker is what would otherwise have started it — which is why
// the call there is guarded on common.Runnable rather than being one more
// worker loop.
func (t *Handler) Start() error {
	tunName := t.config.Name
	tunOptions := TunOptions{
		Name: tunName,
		MTU:  t.config.MTU,
	}
	tunInterface, err := NewTun(tunOptions)
	if err != nil {
		return err
	}

	errors.LogInfo(t.ctx, tunName, " created")

	tunStackOptions := StackOptions{
		Tun:         tunInterface,
		IdleTimeout: t.policyManager.ForLevel(t.config.UserLevel).Timeouts.ConnectionIdle,
	}
	tunStack, err := NewStack(t.ctx, tunStackOptions, t)
	if err != nil {
		_ = tunInterface.Close()
		return err
	}

	err = tunStack.Start()
	if err != nil {
		_ = tunStack.Close()
		_ = tunInterface.Close()
		return err
	}

	err = tunInterface.Start()
	if err != nil {
		_ = tunStack.Close()
		_ = tunInterface.Close()
		return err
	}

	t.stack = tunStack
	t.tun = tunInterface

	errors.LogInfo(t.ctx, tunName, " up")
	return nil
}

// Close implements common.Closable, and it is what lets the tun descriptor go.
//
// Without it the stack built in Init outlives the instance that owns it. Nothing
// else reaches this handler on the way down: it has no listening port, so
// AlwaysOnInboundHandler builds no worker for it, and a worker is what calls
// common.Close on a proxy. The stack's own Close is therefore the only path to
// the gVisor endpoint's Attach(nil) — the one call that stops the dispatchers
// and waits for their goroutines to leave dispatchLoop.
//
// On Android that is not a tidiness question. Those goroutines sit in poll() on
// the descriptor VpnService.Builder established, so the kernel holds a reference
// to the tun file and closing the descriptor on the Java side does not bring the
// interface down. system_server tears the VPN network — and the status bar key
// icon with it — from Vpn.interfaceRemoved, which never fires while the
// interface is still referenced. Measured before this existed: the interface
// outlived the disconnect by 4.7 s, by over 25 s, and once by 31 s, the wait
// being however long it took for some packet to wake the poll.
func (t *Handler) Close() error {
	// Reachable more than once: teardown has several entrances and a failed
	// Init closes what it managed to build.
	t.closeOnce.Lock()
	defer t.closeOnce.Unlock()

	var errs []error
	// The stack first. It is what is reading the descriptor, and its Close
	// blocks until the readers are gone; releasing the tun before them would
	// leave them reading a descriptor that had already been let go.
	if t.stack != nil {
		errs = append(errs, t.stack.Close())
		t.stack = nil
	}
	if t.tun != nil {
		errs = append(errs, t.tun.Close())
		t.tun = nil
	}
	if err := errors.Combine(errs...); err != nil {
		return errors.New("failed to close tun handler").Base(err)
	}
	return nil
}

// HandleConnection pass the connection coming from the ip stack to the routing dispatcher
func (t *Handler) HandleConnection(conn net.Conn, destination net.Destination) {
	// when handling is done with any outcome, always signal back to the incoming connection
	// to close, send completion packets back to the network, and cleanup
	defer conn.Close()

	ctx, cancel := context.WithCancel(t.ctx)
	defer cancel()
	ctx = c.ContextWithID(ctx, session.NewID())

	source := net.DestinationFromAddr(conn.RemoteAddr())
	inbound := session.Inbound{
		Name:          "tun",
		Tag:           t.tag,
		CanSpliceCopy: 3,
		Source:        source,
		User: &protocol.MemoryUser{
			Level: t.config.UserLevel,
		},
	}

	ctx = session.ContextWithInbound(ctx, &inbound)
	ctx = session.ContextWithContent(ctx, &session.Content{
		SniffingRequest: t.sniffingRequest,
	})
	ctx = session.SubContextFromMuxInbound(ctx)

	ctx = log.ContextWithAccessMessage(ctx, &log.AccessMessage{
		From:   inbound.Source,
		To:     destination,
		Status: log.AccessAccepted,
		Reason: "",
	})
	errors.LogInfo(ctx, "processing from ", source, " to ", destination)

	link := &transport.Link{
		Reader: &buf.TimeoutWrapperReader{Reader: buf.NewReader(conn)},
		Writer: buf.NewWriter(conn),
	}
	if err := t.dispatcher.DispatchLink(ctx, destination, link); err != nil {
		errors.LogError(ctx, errors.New("connection closed").Base(err))
	}
}

// Network implements proxy.Inbound
// and exists only to comply to proxy interface, declaring it doesn't listen on any network,
// making the process not open any port for this inbound (input will be network interface)
func (t *Handler) Network() []net.Network {
	return []net.Network{}
}

// Process implements proxy.Inbound
// and exists only to comply to proxy interface, which should never get any inputs due to no listening ports
func (t *Handler) Process(ctx context.Context, network net.Network, conn stat.Connection, dispatcher routing.Dispatcher) error {
	return nil
}

func init() {
	common.Must(common.RegisterConfig((*Config)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		t := &Handler{config: config.(*Config)}
		err := core.RequireFeatures(ctx, func(pm policy.Manager, dispatcher routing.Dispatcher, dnsClient dns.Client) error {
			return t.Init(ctx, pm, dispatcher, dnsClient)
		})
		return t, err
	}))
}
