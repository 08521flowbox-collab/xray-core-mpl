package tun

import (
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/dns"
	"golang.org/x/net/dns/dnsmessage"
)

type fakeDNSClient struct {
	ips []net.IP
	err error
}

func (fakeDNSClient) Type() interface{} { return dns.ClientType() }
func (fakeDNSClient) Start() error      { return nil }
func (fakeDNSClient) Close() error      { return nil }
func (c fakeDNSClient) LookupIP(string, dns.IPOption) ([]net.IP, uint32, error) {
	return c.ips, 60, c.err
}

func dnsQuery(t *testing.T, id uint16, name string, qType dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: qType, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

type capturedWrite struct {
	payload  []byte
	src, dst net.Destination
}

func newCapturingFastPath(client dns.Client) (*dnsFastPath, chan capturedWrite) {
	writes := make(chan capturedWrite, 4)
	fp := &dnsFastPath{client: client, write: func(payload []byte, src, dst net.Destination) error {
		writes <- capturedWrite{payload: append([]byte(nil), payload...), src: src, dst: dst}
		return nil
	}}
	return fp, writes
}

func TestDNSFastPathAnswersAQueryWithoutAFlow(t *testing.T) {
	fp, writes := newCapturingFastPath(fakeDNSClient{ips: []net.IP{net.ParseIP("1.2.3.4")}})
	client := net.UDPDestination(net.ParseAddress("10.0.0.2"), 40000)
	resolver := net.UDPDestination(net.ParseAddress("1.1.1.1"), 53)

	if !fp.Handle(client, resolver, dnsQuery(t, 0x1234, "example.com.", dnsmessage.TypeA)) {
		t.Fatal("an A query was not taken")
	}
	select {
	case w := <-writes:
		if w.src != resolver || w.dst != client {
			t.Fatalf("answer went from %s to %s", w.src.NetAddr(), w.dst.NetAddr())
		}
		var p dnsmessage.Parser
		h, err := p.Start(w.payload)
		if err != nil || h.ID != 0x1234 || !h.Response || h.RCode != dnsmessage.RCodeSuccess {
			t.Fatalf("bad answer header %+v err %v", h, err)
		}
		if err := p.SkipAllQuestions(); err != nil {
			t.Fatal(err)
		}
		if _, err := p.AnswerHeader(); err != nil {
			t.Fatal(err)
		}
		a, err := p.AResource()
		if err != nil || a.A != [4]byte{1, 2, 3, 4} {
			t.Fatalf("answer %v err %v", a, err)
		}
	case <-time.After(time.Second):
		t.Fatal("no answer written")
	}
	if fp.answered.Load() != 1 {
		t.Fatalf("answered = %d", fp.answered.Load())
	}
}

func TestDNSFastPathRefusesNonIPQueries(t *testing.T) {
	fp, writes := newCapturingFastPath(fakeDNSClient{})
	client := net.UDPDestination(net.ParseAddress("10.0.0.2"), 40000)
	resolver := net.UDPDestination(net.ParseAddress("1.1.1.1"), 53)

	if !fp.Handle(client, resolver, dnsQuery(t, 7, "example.com.", dnsmessage.TypeTXT)) {
		t.Fatal("a TXT query was not taken")
	}
	w := <-writes
	var p dnsmessage.Parser
	h, err := p.Start(w.payload)
	if err != nil || h.ID != 7 || h.RCode != dnsmessage.RCodeRefused {
		t.Fatalf("expected REFUSED, got %+v err %v", h, err)
	}
	if fp.refused.Load() != 1 {
		t.Fatalf("refused = %d", fp.refused.Load())
	}
}

func TestDNSFastPathLeavesGarbageToTheFlowPath(t *testing.T) {
	fp, _ := newCapturingFastPath(fakeDNSClient{})
	if fp.Handle(net.UDPDestination(net.LocalHostIP, 1), net.UDPDestination(net.LocalHostIP, 53), []byte{1, 2, 3}) {
		t.Fatal("garbage was taken")
	}
}

func TestHandlePacketUsesTheDNSFastPathBeforeMakingAFlow(t *testing.T) {
	handler := newUdpConnectionHandler(func(conn net.Conn, dest net.Destination) {
		t.Error("a flow was created for a hijacked query")
		conn.Close()
	}, func([]byte, net.Destination, net.Destination) error { return nil }, time.Hour)
	defer handler.Close()
	fp, writes := newCapturingFastPath(fakeDNSClient{ips: []net.IP{net.ParseIP("1.2.3.4")}})
	handler.hijackDNS = fp.Handle

	handler.HandlePacket(net.UDPDestination(net.LocalHostIP, 40000), net.UDPDestination(net.ParseAddress("1.1.1.1"), 53), dnsQuery(t, 1, "example.com.", dnsmessage.TypeA))
	select {
	case <-writes:
	case <-time.After(time.Second):
		t.Fatal("no answer written")
	}
	handler.RLock()
	flows := len(handler.udpConns)
	handler.RUnlock()
	if flows != 0 {
		t.Fatalf("flows = %d, expected none", flows)
	}
}
