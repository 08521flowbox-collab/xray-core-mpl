package internet

import (
	"sync"
	"sync/atomic"

	"github.com/xtls/xray-core/common/net"
)

// dialAddresses maps an outbound tag to the IPs its server can be reached at.
//
// An outbound's server is one address in its configuration, and this is how a
// server with two — one per family — is offered to Happy Eyeballs without the
// configuration naming a domain the DNS app would have to resolve. It is a
// process-wide table like dnsClient and effectiveSystemDialer are: the caller
// that owns the outbound registers the addresses before adding it and clears
// them after removing it, and DialSystem consults the table by the tag the
// dispatcher stamped on the context.
var dialAddresses sync.Map

func SetDialAddresses(tag string, ips []net.IP) {
	if tag == "" || len(ips) == 0 {
		return
	}
	dialAddresses.Store(tag, append([]net.IP(nil), ips...))
}

func ClearDialAddresses(tag string) {
	dialAddresses.Delete(tag)
}

// ResetDialAddresses empties the table. Handlers clear their own tag when they
// are removed, but the last set of a core that is shut down whole stays behind;
// a long-lived process building core after core would otherwise keep one entry
// per tag it has ever used.
func ResetDialAddresses() {
	dialAddresses.Range(func(key, _ any) bool {
		dialAddresses.Delete(key)
		return true
	})
}

func DialAddresses(tag string) []net.IP {
	if tag == "" {
		return nil
	}
	ips, ok := dialAddresses.Load(tag)
	if !ok {
		return nil
	}
	return ips.([]net.IP)
}

// Unlike the DNS-driven entry in DialSystem, TryDelayMs may be zero here: every
// registered address is dialled at once and the first to connect wins.
func raceable(sockopt *SocketConfig, network net.Network, ips []net.IP) bool {
	return sockopt != nil && sockopt.HappyEyeballs != nil &&
		sockopt.HappyEyeballs.MaxConcurrentTry > 0 &&
		len(sockopt.DialerProxy) == 0 && network == net.Network_TCP && len(ips) >= 2
}

// raceWins counts, per address family, the dials the table above has decided.
// Nothing in this process reads it; the embedding application does, to learn
// whether the second address ever carries anything.
var raceWins struct{ v4, v6 atomic.Uint64 }

func recordRaceWin(addr net.Addr) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return
	}
	if tcp.IP.To4() != nil {
		raceWins.v4.Add(1)
		return
	}
	raceWins.v6.Add(1)
}

func RaceWins() (v4, v6 uint64) {
	return raceWins.v4.Load(), raceWins.v6.Load()
}

func ResetRaceWins() {
	raceWins.v4.Store(0)
	raceWins.v6.Store(0)
}
