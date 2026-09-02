package tun

import (
	"context"
	go_errors "errors"
	"sync/atomic"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/dns"
	"golang.org/x/net/dns/dnsmessage"
)

// dnsFastPath answers UDP queries to port 53 straight out of the DNS client,
// the way sing-box's tun does, instead of minting a NAT flow, a routing
// decision and a proxy/dns session for each one. The answers are the same
// ones proxy/dns would give with its default config — A and AAAA out of
// dns.Client with FakeDNS enabled, everything else REFUSED — so the host's
// port-53 rule keeps its meaning; only the machinery behind it is gone.
// Measured on the iOS packet tunnel: 198 finished queries were holding three
// goroutines and an egress queue each at the moment the process hit 40 MB.
type dnsFastPath struct {
	client dns.Client
	write  func(payload []byte, src, dst net.Destination) error

	answered atomic.Uint64
	refused  atomic.Uint64
	failed   atomic.Uint64
}

// Handle reports whether it took the packet. A payload that does not parse as
// a DNS query is left to the ordinary flow path, which will do with it what it
// always did.
func (f *dnsFastPath) Handle(src, dst net.Destination, payload []byte) bool {
	var parser dnsmessage.Parser
	header, err := parser.Start(payload)
	if err != nil || header.Response {
		return false
	}
	q, err := parser.Question()
	if err != nil {
		return false
	}
	if q.Class != dnsmessage.ClassINET || (q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeAAAA) {
		f.refused.Add(1)
		f.reply(src, dst, buildDNSReply(header.ID, q, dnsmessage.RCodeRefused, nil, 0))
		return true
	}
	go f.lookup(src, dst, header.ID, q)
	return true
}

func (f *dnsFastPath) lookup(src, dst net.Destination, id uint16, q dnsmessage.Question) {
	domain := q.Name.String()
	ips, ttl, err := f.client.LookupIP(domain, dns.IPOption{
		IPv4Enable: q.Type == dnsmessage.TypeA,
		IPv6Enable: q.Type == dnsmessage.TypeAAAA,
		FakeEnable: true,
	})
	rcode := dns.RCodeFromError(err)
	if rcode == 0 && len(ips) == 0 && !go_errors.Is(err, dns.ErrEmptyResponse) {
		f.failed.Add(1)
		errors.LogInfoInner(context.Background(), err, "tun dns: lookup ", domain)
		return
	}
	f.answered.Add(1)
	f.reply(src, dst, buildDNSReply(id, q, dnsmessage.RCode(rcode), ips, ttl))
}

// reply writes the answer back the way udpConn.Write does: source and
// destination swapped, so the client sees it come from the resolver it asked.
func (f *dnsFastPath) reply(src, dst net.Destination, msg []byte) {
	if msg == nil {
		return
	}
	if err := f.write(msg, dst, src); err != nil {
		errors.LogInfoInner(context.Background(), err, "tun dns: write answer to ", src.NetAddr())
	}
}

func buildDNSReply(id uint16, q dnsmessage.Question, rcode dnsmessage.RCode, ips []net.IP, ttl uint32) []byte {
	builder := dnsmessage.NewBuilder(make([]byte, 0, 512), dnsmessage.Header{
		ID:                 id,
		RCode:              rcode,
		RecursionAvailable: true,
		RecursionDesired:   true,
		Response:           true,
		Authoritative:      true,
	})
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return nil
	}
	if err := builder.Question(q); err != nil {
		return nil
	}
	if err := builder.StartAnswers(); err != nil {
		return nil
	}
	rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: ttl}
	for _, ip := range ips {
		ip4 := ip.To4()
		switch {
		case q.Type == dnsmessage.TypeA && ip4 != nil:
			var r dnsmessage.AResource
			copy(r.A[:], ip4)
			if err := builder.AResource(rh, r); err != nil {
				return nil
			}
		case q.Type == dnsmessage.TypeAAAA && ip4 == nil && len(ip) == net.IPv6len:
			var r dnsmessage.AAAAResource
			copy(r.AAAA[:], ip)
			if err := builder.AAAAResource(rh, r); err != nil {
				return nil
			}
		}
	}
	msg, err := builder.Finish()
	if err != nil {
		return nil
	}
	return msg
}
