package tun

import (
	"context"
	"testing"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/policy"
)

// Init used to create the tun and start the ip stack, which put the interface
// on the wire while core.Start had not yet started the features. The first
// packet was then dispatched into a FakeDNS holder whose ipRange was still nil
// and took the process down inside net.(*IPNet).Contains — upstream #6442,
// closed as a tun-starts-too-early problem rather than a missing nil guard.
//
// Cherry-picked fix from upstream #6275. This pins the property the fix exists
// for: after Init the handler owns no interface, and only Start may create one.
func TestInitDoesNotBringTheInterfaceUp(t *testing.T) {
	instance, err := core.New(&core.Config{})
	common.Must(err)
	// core.XrayKey is exported for exactly this: Init detaches the context from
	// the instance, and there is no other way in from outside package core.
	ctx := context.WithValue(context.Background(), core.XrayKey(1), instance)

	handler := &Handler{config: &Config{Name: "zaptun-test", MTU: 9000}}
	common.Must(handler.Init(ctx, policy.DefaultManager{}, nil))

	if handler.tun != nil {
		t.Fatal("Init created a tun interface; only Start may")
	}
	if handler.stack != nil {
		t.Fatal("Init created an ip stack; only Start may")
	}
}

// The ordering fix only works if AlwaysOnInboundHandler can see the handler as
// something to start. A tun inbound has no listening port and therefore no
// worker, so this interface is the only thing that reaches it.
func TestHandlerIsRunnable(t *testing.T) {
	var _ common.Runnable = (*Handler)(nil)
}
