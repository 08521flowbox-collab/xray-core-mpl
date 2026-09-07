package router_test

import (
	"net/netip"
	"testing"

	. "github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/net"
)

func ip(s string) net.IP { return net.ParseAddress(s).IP() }

func TestPrefixSetMatcher(t *testing.T) {
	m := NewPrefixSetMatcher([]netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("10.1.0.0/16"),
		netip.MustParsePrefix("192.168.1.5/24"),
		netip.MustParsePrefix("192.168.2.0/24"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2001:db8:1::/48"),
		netip.MustParsePrefix("2001:db9::/32"),
		netip.MustParsePrefix("2400:cb00::/32"),
	}, false)
	if m.Len() != 6 {
		t.Fatalf("Len=%d want 6 merged ranges", m.Len())
	}
	cases := map[string]bool{
		"10.200.3.4":           true,
		"11.0.0.0":             false,
		"9.255.255.255":        false,
		"192.168.1.0":          true,
		"192.168.2.255":        true,
		"192.168.3.0":          false,
		"2001:db8::1":          true,
		"2001:db8:ffff::1":     true,
		"2001:db9:ffff:ffff::": true,
		"2001:dba::":           false,
		"2001:db7:ffff::1":     false,
		"2400:cb00::1":         true,
		"2400:cb01::1":         false,
		"::ffff:10.0.0.1":      true,
		"::ffff:11.0.0.1":      false,
	}
	for s, want := range cases {
		if got := m.Match(ip(s)); got != want {
			t.Errorf("Match(%s)=%v want %v", s, got, want)
		}
	}
	m.SetReverse(true)
	if m.Match(ip("10.0.0.1")) || !m.Match(ip("11.0.0.1")) {
		t.Error("reverse did not flip the answer")
	}
	m.ToggleReverse()
	if !m.Matches([]net.IP{ip("10.0.0.1"), ip("192.168.2.1")}) {
		t.Error("Matches should be true when every address is inside")
	}
	if m.Matches([]net.IP{ip("10.0.0.1"), ip("8.8.8.8")}) {
		t.Error("Matches should be false when one address is outside")
	}
	if !m.AnyMatch([]net.IP{ip("8.8.8.8"), ip("10.0.0.1")}) {
		t.Error("AnyMatch missed")
	}
	in, out := m.FilterIPs([]net.IP{ip("10.0.0.1"), ip("8.8.8.8")})
	if len(in) != 1 || len(out) != 1 {
		t.Errorf("FilterIPs split %d/%d", len(in), len(out))
	}
}

func TestPrefixSetMatcherWholeSpace(t *testing.T) {
	m := NewPrefixSetMatcher([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, false)
	for _, s := range []string{"0.0.0.0", "255.255.255.255", "127.0.0.1"} {
		if !m.Match(ip(s)) {
			t.Errorf("%s not matched by the whole space", s)
		}
	}
	if m.Match(ip("2001:db8::1")) {
		t.Error("0.0.0.0/0 matched an IPv6 address")
	}
	m6 := NewPrefixSetMatcher([]netip.Prefix{netip.MustParsePrefix("::/0")}, false)
	for _, s := range []string{"::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "2001:db8::1"} {
		if !m6.Match(ip(s)) {
			t.Errorf("%s not matched by the whole v6 space", s)
		}
	}
	if m6.Match(ip("10.0.0.1")) {
		t.Error("::/0 matched an IPv4 address")
	}
}

func TestPrefixSetMatcherV6Boundaries(t *testing.T) {
	cases := []struct {
		prefix string
		in     []string
		out    []string
	}{
		{"2001:db8::/64", []string{"2001:db8::", "2001:db8::ffff:ffff:ffff:ffff"}, []string{"2001:db8:0:1::", "2001:db7:ffff:ffff:ffff:ffff:ffff:ffff"}},
		{"2001:db8::/63", []string{"2001:db8:0:1:ffff:ffff:ffff:ffff"}, []string{"2001:db8:0:2::"}},
		{"2001:db8::/65", []string{"2001:db8::7fff:ffff:ffff:ffff"}, []string{"2001:db8::8000:0:0:0"}},
		{"2001:db8::1/128", []string{"2001:db8::1"}, []string{"2001:db8::", "2001:db8::2"}},
		{"8000::/1", []string{"8000::", "ffff::1"}, []string{"7fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}},
	}
	for _, c := range cases {
		m := NewPrefixSetMatcher([]netip.Prefix{netip.MustParsePrefix(c.prefix)}, false)
		for _, s := range c.in {
			if !m.Match(ip(s)) {
				t.Errorf("%s should be inside %s", s, c.prefix)
			}
		}
		for _, s := range c.out {
			if m.Match(ip(s)) {
				t.Errorf("%s should be outside %s", s, c.prefix)
			}
		}
	}
}

func TestNewPrefixIPMatcherIncludeAndExclude(t *testing.T) {
	m, err := NewPrefixIPMatcher([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, MatcherAsType_Target)
	if err != nil || m == nil {
		t.Fatalf("matcher %v err %v", m, err)
	}
	if _, err := NewPrefixIPMatcher(nil, nil, MatcherAsType_Target); err == nil {
		t.Error("empty lists accepted")
	}
}
