package internet

import (
	"sync"

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

func dialAddressesFor(tag string) []net.IP {
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
