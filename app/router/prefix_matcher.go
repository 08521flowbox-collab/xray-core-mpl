package router

import (
	"encoding/binary"
	"net/netip"
	"sort"

	"github.com/xtls/xray-core/common/net"
)

// PrefixSetMatcher answers GeoIPMatcher from sorted, merged ranges searched by
// bisection: eight bytes per IPv4 range, thirty-two per IPv6 range. A
// country's geoip entry is hundreds of thousands of prefixes, and the
// general-purpose IP set costs several times this to build and hold.
type PrefixSetMatcher struct {
	v4      []rangeV4
	v6      []rangeV6
	reverse bool
}

type rangeV4 struct{ lo, hi uint32 }

type u128 struct{ hi, lo uint64 }

func (a u128) less(b u128) bool { return a.hi < b.hi || (a.hi == b.hi && a.lo < b.lo) }

func (a u128) lessOrEqual(b u128) bool { return !b.less(a) }

func u128Of(addr netip.Addr) u128 {
	b := addr.As16()
	return u128{binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])}
}

type rangeV6 struct{ lo, hi u128 }

func NewPrefixSetMatcher(prefixes []netip.Prefix, reverse bool) *PrefixSetMatcher {
	m := &PrefixSetMatcher{reverse: reverse}
	n4, n6 := 0, 0
	for _, p := range prefixes {
		switch {
		case !p.IsValid():
		case p.Addr().Is4():
			n4++
		default:
			n6++
		}
	}
	m.v4 = make([]rangeV4, 0, n4)
	m.v6 = make([]rangeV6, 0, n6)
	for _, p := range prefixes {
		if !p.IsValid() {
			continue
		}
		p = p.Masked()
		if p.Addr().Is4() {
			lo := binary.BigEndian.Uint32(p.Addr().AsSlice())
			hi := lo | (^uint32(0) >> uint(p.Bits()))
			m.v4 = append(m.v4, rangeV4{lo, hi})
			continue
		}
		lo := u128Of(p.Addr())
		hi := lo
		if bits := uint(p.Bits()); bits < 64 {
			hi.hi |= ^uint64(0) >> bits
			hi.lo = ^uint64(0)
		} else {
			hi.lo |= ^uint64(0) >> (bits - 64)
		}
		m.v6 = append(m.v6, rangeV6{lo, hi})
	}
	sort.Slice(m.v4, func(i, j int) bool { return m.v4[i].lo < m.v4[j].lo })
	m.v4 = mergeV4(m.v4)
	sort.Slice(m.v6, func(i, j int) bool { return m.v6[i].lo.less(m.v6[j].lo) })
	m.v6 = mergeV6(m.v6)
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

func mergeV6(in []rangeV6) []rangeV6 {
	out := in[:0]
	for _, r := range in {
		if n := len(out); n > 0 && r.lo.lessOrEqual(out[n-1].hi) {
			if out[n-1].hi.less(r.hi) {
				out[n-1].hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// Len reports the number of merged ranges, for logs.
func (m *PrefixSetMatcher) Len() int { return len(m.v4) + len(m.v6) }

func (m *PrefixSetMatcher) contains(a netip.Addr) bool {
	if a.Is4In6() {
		a = a.Unmap()
	}
	if a.Is4() {
		x := binary.BigEndian.Uint32(a.AsSlice())
		i := sort.Search(len(m.v4), func(i int) bool { return m.v4[i].lo > x })
		return i > 0 && x <= m.v4[i-1].hi
	}
	x := u128Of(a)
	i := sort.Search(len(m.v6), func(i int) bool { return x.less(m.v6[i].lo) })
	return i > 0 && x.lessOrEqual(m.v6[i-1].hi)
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
