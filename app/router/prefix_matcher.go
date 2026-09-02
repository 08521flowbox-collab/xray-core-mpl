package router

import (
	"encoding/binary"
	"net/netip"
	"sort"

	"github.com/xtls/xray-core/common/net"
)

// PrefixSetMatcher answers GeoIPMatcher from sorted, merged IPv4 ranges:
// eight bytes per range, searched by bisection. A country's geoip entry is
// hundreds of thousands of prefixes, and the general-purpose IP set costs
// several times this to build and hold. IPv6 prefixes are dropped: the
// consumer of this fork blocks IPv6 wholesale, so no rule can match one.
type PrefixSetMatcher struct {
	v4      []rangeV4
	reverse bool
}

type rangeV4 struct{ lo, hi uint32 }

func NewPrefixSetMatcher(prefixes []netip.Prefix, reverse bool) *PrefixSetMatcher {
	m := &PrefixSetMatcher{reverse: reverse}
	for _, p := range prefixes {
		if !p.IsValid() || !p.Addr().Is4() {
			continue
		}
		p = p.Masked()
		lo := binary.BigEndian.Uint32(p.Addr().AsSlice())
		hi := lo | (^uint32(0) >> uint(p.Bits()))
		m.v4 = append(m.v4, rangeV4{lo, hi})
	}
	sort.Slice(m.v4, func(i, j int) bool { return m.v4[i].lo < m.v4[j].lo })
	m.v4 = mergeV4(m.v4)
	return m
}

func mergeV4(in []rangeV4) []rangeV4 {
	out := in[:0]
	for _, r := range in {
		if n := len(out); n > 0 && r.lo <= out[n-1].hi {
			if r.hi > out[n-1].hi {
				out[n-1].hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// Len reports the number of merged ranges, for logs.
func (m *PrefixSetMatcher) Len() int { return len(m.v4) }

func (m *PrefixSetMatcher) contains(a netip.Addr) bool {
	if a.Is4In6() {
		a = a.Unmap()
	}
	if !a.Is4() {
		return false
	}
	x := binary.BigEndian.Uint32(a.AsSlice())
	i := sort.Search(len(m.v4), func(i int) bool { return m.v4[i].lo > x })
	return i > 0 && x <= m.v4[i-1].hi
}

func (m *PrefixSetMatcher) matchAddr(a netip.Addr) bool {
	return m.contains(a) != m.reverse
}

func (m *PrefixSetMatcher) Match(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return ok && m.matchAddr(a)
}

func (m *PrefixSetMatcher) AnyMatch(ips []net.IP) bool {
	for _, ip := range ips {
		if m.Match(ip) {
			return true
		}
	}
	return false
}

func (m *PrefixSetMatcher) Matches(ips []net.IP) bool {
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip)
		if !ok || !m.matchAddr(a) {
			return false
		}
	}
	return true
}

func (m *PrefixSetMatcher) FilterIPs(ips []net.IP) (matched []net.IP, unmatched []net.IP) {
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		if m.matchAddr(a) {
			matched = append(matched, ip)
		} else {
			unmatched = append(unmatched, ip)
		}
	}
	return matched, unmatched
}

func (m *PrefixSetMatcher) ToggleReverse() { m.reverse = !m.reverse }

func (m *PrefixSetMatcher) SetReverse(reverse bool) { m.reverse = reverse }

// anyGeoIPMatcher ORs several matchers, the way a rule lists several geoip
// entries.
type anyGeoIPMatcher struct{ subs []GeoIPMatcher }

func (mm *anyGeoIPMatcher) Match(ip net.IP) bool {
	for _, m := range mm.subs {
		if m.Match(ip) {
			return true
		}
	}
	return false
}

func (mm *anyGeoIPMatcher) AnyMatch(ips []net.IP) bool {
	for _, ip := range ips {
		if mm.Match(ip) {
			return true
		}
	}
	return false
}

func (mm *anyGeoIPMatcher) Matches(ips []net.IP) bool {
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !mm.Match(ip) {
			return false
		}
	}
	return true
}

func (mm *anyGeoIPMatcher) FilterIPs(ips []net.IP) (matched []net.IP, unmatched []net.IP) {
	for _, ip := range ips {
		if _, ok := netip.AddrFromSlice(ip); !ok {
			continue
		}
		if mm.Match(ip) {
			matched = append(matched, ip)
		} else {
			unmatched = append(unmatched, ip)
		}
	}
	return matched, unmatched
}

func (mm *anyGeoIPMatcher) ToggleReverse() {
	for _, m := range mm.subs {
		m.ToggleReverse()
	}
}

func (mm *anyGeoIPMatcher) SetReverse(reverse bool) {
	for _, m := range mm.subs {
		m.SetReverse(reverse)
	}
}
