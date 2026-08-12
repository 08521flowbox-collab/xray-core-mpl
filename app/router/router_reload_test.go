package router_test

import (
	"context"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"
	. "github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/routing"
	routing_session "github.com/xtls/xray-core/features/routing/session"
	"github.com/xtls/xray-core/testing/mocks"
)

// newTestRouter builds a Router over mocks and hands back the DNS client so a
// test that expects a lookup can say so. Nothing is expected by default, which
// makes an unintended resolution a failure rather than a silent success.
func newTestRouter(t *testing.T, config *Config) (*Router, *mocks.DNSClient) {
	t.Helper()

	mockCtl := gomock.NewController(t)
	t.Cleanup(mockCtl.Finish)

	mockDNS := mocks.NewDNSClient(mockCtl)
	r := new(Router)
	common.Must(r.Init(context.TODO(), config, mockDNS, &mockOutboundManager{
		Manager:         mocks.NewOutboundManager(mockCtl),
		HandlerSelector: mocks.NewOutboundHandlerSelector(mockCtl),
	}, nil))
	return r, mockDNS
}

// tcpToDomain is the shape of context PickRoute sees on the tun path: the
// sniffer has already put a domain into the destination.
func tcpToDomain(domain string) routing.Context {
	ctx := session.ContextWithOutbounds(context.Background(), []*session.Outbound{{
		Target: net.TCPDestination(net.DomainAddress(domain), 443),
	}})
	return routing_session.AsRoutingContext(ctx)
}

func ruleOn(network net.Network, tag string) *RoutingRule {
	return &RoutingRule{
		TargetTag: &RoutingRule_Tag{Tag: tag},
		Networks:  []net.Network{network},
	}
}

// networkRule matches by network, which keeps most of these tests away from the
// domain and geoip matchers — BuildCondition consumes those fields, so a config
// carrying them is only good for a single build.
func networkRule(tag string) *Config {
	return &Config{Rule: []*RoutingRule{ruleOn(net.Network_TCP, tag)}}
}

// geoipRule matches an address, so it can only ever fire against a domain
// target when the strategy resolves it first.
func geoipRule(strategy Config_DomainStrategy, tag string) *Config {
	return &Config{
		DomainStrategy: strategy,
		Rule: []*RoutingRule{{
			TargetTag: &RoutingRule_Tag{Tag: tag},
			Geoip:     []*GeoIP{{Cidr: []*CIDR{{Ip: []byte{192, 168, 0, 0}, Prefix: 16}}}},
		}},
	}
}

func expectLookup(m *mocks.DNSClient, domain string, ip net.IP) {
	m.EXPECT().LookupIP(gomock.Eq(domain), dns.IPOption{
		IPv4Enable: true,
		IPv6Enable: true,
		FakeEnable: false,
	}).Return([]net.IP{ip}, uint32(600), nil).AnyTimes()
}

// TestReloadRacesPickRoute is the reason the rule set became a snapshot.
// PickRoute walks the rules without holding anything, so a reload replacing
// that slice is an unsynchronised write to a field a reader is walking. This
// fails under -race on the unfixed router. The fix is not "add a read lock" —
// see MODIFICATIONS.md for why that is worse here than the race it removes.
func TestReloadRacesPickRoute(t *testing.T) {
	r, _ := newTestRouter(t, networkRule("first"))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r.PickRoute(tcpToDomain("example.com"))
			}
		}()
	}

	for i := 0; i < 500; i++ {
		// A fresh config every round: BuildCondition mutates the rules it
		// compiles, so a reused one is only good for a single reload.
		common.Must(r.ReloadRules(networkRule("second"), false))
	}

	close(stop)
	wg.Wait()
}

// TestFailedReloadKeepsThePreviousRules pins the rollback. Upstream clears the
// live set before it starts building, so a config that fails halfway leaves the
// router with whatever it had managed to compile; here nothing is published
// until everything is built.
func TestFailedReloadKeepsThePreviousRules(t *testing.T) {
	cases := []struct {
		name   string
		config *Config
	}{
		{
			// BuildCondition rejects it: "this rule has no effective fields".
			name:   "a rule that compiles to nothing",
			config: &Config{Rule: []*RoutingRule{{TargetTag: &RoutingRule_Tag{Tag: "second"}}}},
		},
		{
			// Rejected against the rules built so far in this same call, which
			// is the only set a duplicate within one config can collide with.
			name: "two rules sharing a ruleTag",
			config: &Config{Rule: []*RoutingRule{
				{TargetTag: &RoutingRule_Tag{Tag: "second"}, RuleTag: "dup", Networks: []net.Network{net.Network_TCP}},
				{TargetTag: &RoutingRule_Tag{Tag: "third"}, RuleTag: "dup", Networks: []net.Network{net.Network_TCP}},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRouter(t, networkRule("first"))

			if err := r.ReloadRules(tc.config, false); err == nil {
				t.Fatal("expected the reload to be rejected")
			}

			route, err := r.PickRoute(tcpToDomain("example.com"))
			common.Must(err)
			if tag := route.GetOutboundTag(); tag != "first" {
				t.Fatalf("routes to %q after a failed reload, want the pre-reload %q", tag, "first")
			}
			if n := len(r.ListRule()); n != 1 {
				t.Fatalf("rule set has %d rules after a failed reload, want the pre-reload 1", n)
			}
		})
	}
}

// TestReloadUpdatesDomainStrategy covers the one behaviour this fork adds
// rather than repairs: upstream fixes the strategy at Init and ReloadRules
// cannot reach it, so IpIfNonMatch can never be turned on for the address rules
// that need it and never turned off again when they are gone.
func TestReloadUpdatesDomainStrategy(t *testing.T) {
	r, mockDNS := newTestRouter(t, geoipRule(Config_AsIs, "matched"))
	expectLookup(mockDNS, "example.com", net.IP{192, 168, 0, 1})

	if _, err := r.PickRoute(tcpToDomain("example.com")); err == nil {
		t.Fatal("under AsIs an address rule must not match a domain target")
	}

	common.Must(r.ReloadRules(geoipRule(Config_IpIfNonMatch, "matched"), false))

	route, err := r.PickRoute(tcpToDomain("example.com"))
	common.Must(err)
	if tag := route.GetOutboundTag(); tag != "matched" {
		t.Fatalf("routes to %q, want %q — the reloaded IpIfNonMatch did not take", tag, "matched")
	}

	common.Must(r.ReloadRules(geoipRule(Config_AsIs, "matched"), false))
	if _, err := r.PickRoute(tcpToDomain("example.com")); err == nil {
		t.Fatal("the strategy did not go back to AsIs")
	}
}

// TestAppendingRulesKeepsTheStrategy guards the asymmetry: a full replacement
// takes the incoming strategy, an append keeps the live one. An appended config
// carries DomainStrategy zero (AsIs) whether or not its author meant to say
// anything about the strategy, so reading it would silently disable resolution
// for the rules already loaded.
func TestAppendingRulesKeepsTheStrategy(t *testing.T) {
	r, mockDNS := newTestRouter(t, geoipRule(Config_IpIfNonMatch, "matched"))
	expectLookup(mockDNS, "example.com", net.IP{192, 168, 0, 1})

	common.Must(r.ReloadRules(&Config{Rule: []*RoutingRule{ruleOn(net.Network_UDP, "udp")}}, true))

	if n := len(r.ListRule()); n != 2 {
		t.Fatalf("rule set has %d rules after an append, want 2", n)
	}
	route, err := r.PickRoute(tcpToDomain("example.com"))
	common.Must(err)
	if tag := route.GetOutboundTag(); tag != "matched" {
		t.Fatalf("routes to %q, want %q — the append reset the strategy to AsIs", tag, "matched")
	}
}
