package router_test

import (
	"testing"

	. "github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
)

// catchAllConfig is an address rule with a catch-all behind it — the shape this
// fork's consumer publishes: user rules first, one final rule that matches every
// connection. BuildCondition consumes the fields, so every test builds a fresh
// one.
func catchAllConfig(strategy Config_DomainStrategy) *Config {
	return &Config{
		DomainStrategy: strategy,
		Rule: []*RoutingRule{
			{
				TargetTag: &RoutingRule_Tag{Tag: "geo"},
				Geoip:     []*GeoIP{{Cidr: []*CIDR{{Ip: []byte{192, 168, 0, 0}, Prefix: 16}}}},
			},
			{
				TargetTag: &RoutingRule_Tag{Tag: "fallback"},
				Networks:  []net.Network{net.Network_TCP},
			},
		},
	}
}

// A catch-all match is provisional under IpIfNonMatch: with it in place "no
// rule matched" can never occur, so the resolving second pass upstream hangs
// from that condition would never run — and an address rule above the catch-all
// could never fire against a domain target. The fork resolves on a last-rule
// match and lets an earlier rule take the connection.
func TestCatchAllMatchIsProvisionalUnderIpIfNonMatch(t *testing.T) {
	r, mockDNS := newTestRouter(t, catchAllConfig(Config_IpIfNonMatch))
	expectLookup(mockDNS, "example.com", net.IP{192, 168, 0, 1})

	route, err := r.PickRoute(tcpToDomain("example.com"))
	common.Must(err)
	if tag := route.GetOutboundTag(); tag != "geo" {
		t.Fatalf("routes to %q, want %q — the address rule above the catch-all never saw the resolved address", tag, "geo")
	}
}

// When the resolved address matches nothing above it, the catch-all keeps the
// connection it already claimed.
func TestCatchAllKeepsTheConnectionWhenResolutionMatchesNothing(t *testing.T) {
	r, mockDNS := newTestRouter(t, catchAllConfig(Config_IpIfNonMatch))
	expectLookup(mockDNS, "example.com", net.IP{10, 0, 0, 1})

	route, err := r.PickRoute(tcpToDomain("example.com"))
	common.Must(err)
	if tag := route.GetOutboundTag(); tag != "fallback" {
		t.Fatalf("routes to %q, want %q", tag, "fallback")
	}
}

// Under AsIs nothing resolves. The mock expects no lookup, so an unintended
// resolution fails the test rather than passing silently.
func TestCatchAllDoesNotResolveUnderAsIs(t *testing.T) {
	r, _ := newTestRouter(t, catchAllConfig(Config_AsIs))

	route, err := r.PickRoute(tcpToDomain("example.com"))
	common.Must(err)
	if tag := route.GetOutboundTag(); tag != "fallback" {
		t.Fatalf("routes to %q, want %q", tag, "fallback")
	}
}

// A match on any rule that is not the last one is final, exactly as before:
// resolution is the price of falling through to the catch-all, not of routing.
func TestAMatchAboveTheCatchAllDoesNotResolve(t *testing.T) {
	config := &Config{
		DomainStrategy: Config_IpIfNonMatch,
		Rule: []*RoutingRule{
			{
				TargetTag: &RoutingRule_Tag{Tag: "by-name"},
				Domain:    []*Domain{{Type: Domain_Full, Value: "example.com"}},
			},
			{
				TargetTag: &RoutingRule_Tag{Tag: "fallback"},
				Networks:  []net.Network{net.Network_TCP},
			},
		},
	}
	r, _ := newTestRouter(t, config)

	route, err := r.PickRoute(tcpToDomain("example.com"))
	common.Must(err)
	if tag := route.GetOutboundTag(); tag != "by-name" {
		t.Fatalf("routes to %q, want %q", tag, "by-name")
	}
}
