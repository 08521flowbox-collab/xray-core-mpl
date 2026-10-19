package tun

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
)

// newPeers is a peer for each pair of addresses, all on one link
func newPeers(t *testing.T, pairs ...[2]string) ([]*peer, *link) {
	l := newLink()
	peers := make([]*peer, len(pairs))
	byPort := make(map[uint16]*peer)
	for i, pair := range pairs {
		p := &peer{t: t, l: l, sent: make(chan []byte, 1024), src: netip.MustParseAddrPort(pair[0]), dst: netip.MustParseAddrPort(pair[1]), seq: 1000, window: 0xffff}
		peers[i], byPort[p.src.Port()] = p, p
	}
	l.out = func(b []byte) {
		packet, ok := parsePacket(b)
		if !ok || packet.proto != protoTCP || len(packet.payload) < tcpHeaderSize {
			t.Error("not a segment: ", b)
			return
		}
		byPort[binary.BigEndian.Uint16(packet.payload[2:])].sent <- b
	}
	return peers, l
}

type tableEvents struct {
	accepted, read, released chan uint16
}

// newTableStack is a stack whose connections go through a table of max of them, as they do on Android.
// Its handler reads what comes and holds a connection until the table lets it go
func newTableStack(t *testing.T, l *link, max int) (*lightStack, *tableEvents) {
	ev := &tableEvents{accepted: make(chan uint16, 16), read: make(chan uint16, 16), released: make(chan uint16, 16)}
	s := &lightStack{ctx: t.Context(), tun: l, mtu: testMTU, window: tcpWindow, conns: make(map[flow]*tcpConn), tcp: newTCPTable(max)}
	s.handle = s.tracked(func(ctx context.Context, conn net.Conn, _ net.Destination) {
		defer conn.Close()
		port := uint16(net.DestinationFromAddr(conn.RemoteAddr()).Port)
		ev.accepted <- port
		go func() {
			for {
				mb, err := conn.(buf.Reader).ReadMultiBuffer()
				if !mb.IsEmpty() {
					ev.read <- port
				}
				buf.ReleaseMulti(mb)
				if err != nil {
					return
				}
			}
		}()
		<-ctx.Done()
		ev.released <- port
	})
	s.udp = newUdpConnectionHandler(func(conn net.Conn, _ net.Destination) { conn.Close() }, s.writeUDP, time.Minute)
	s.Start()
	t.Cleanup(func() {
		s.Close()
		l.Close()
	})
	return s, ev
}

func expect(t *testing.T, ch chan uint16, want uint16, what string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("%s: got port %d, want %d", what, got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nothing from port %d", what, want)
	}
}

// The connection that a full table closes for a new one is the one quiet longest, not the oldest, and the system
// is told that it ended.
func TestLightTableClosesTheQuietestNotTheOldest(t *testing.T) {
	peers, l := newPeers(t,
		[2]string{"10.0.0.2:40001", "1.2.3.4:443"},
		[2]string{"10.0.0.2:40002", "1.2.3.4:443"},
		[2]string{"10.0.0.2:40003", "1.2.3.4:443"})
	talker, quiet, newcomer := peers[0], peers[1], peers[2]
	s, ev := newTableStack(t, l, 2)

	talker.connect(nil)
	expect(t, ev.accepted, 40001, "first connection")
	quiet.connect(nil)
	expect(t, ev.accepted, 40002, "second connection")

	talker.send(talker.seq, tcpAck|tcpPsh, nil, []byte("ping"))
	talker.seq += 4
	expect(t, ev.read, 40001, "data from the first connection")

	newcomer.connect(nil)
	expect(t, ev.accepted, 40003, "third connection")
	expect(t, ev.released, 40002, "eviction")
	select {
	case port := <-ev.released:
		t.Fatalf("the connection from port %d was also let go; only the quiet one should be", port)
	case <-time.After(200 * time.Millisecond):
	}

	for {
		seg := quiet.receive()
		if seg.flags&(tcpFin|tcpRst) != 0 {
			break
		}
	}

	c := s.census(0)
	if c.TCP != 2 || c.TCPEvicted != 1 {
		t.Fatalf("census = %d open, %d evicted; want 2 and 1", c.TCP, c.TCPEvicted)
	}
}

// Without a limit every connection stays.
func TestLightTableWithoutLimitNeverCloses(t *testing.T) {
	peers, l := newPeers(t,
		[2]string{"10.0.0.2:40001", "1.2.3.4:443"},
		[2]string{"10.0.0.2:40002", "1.2.3.4:443"},
		[2]string{"10.0.0.2:40003", "1.2.3.4:443"})
	s, ev := newTableStack(t, l, 0)
	for i, p := range peers {
		p.connect(nil)
		expect(t, ev.accepted, uint16(40001+i), "connection")
	}
	if c := s.census(0); c.TCP != 3 || c.TCPEvicted != 0 {
		t.Fatalf("census = %d open, %d evicted; want 3 and 0", c.TCP, c.TCPEvicted)
	}
}

// The census ranks the destinations by their connections, each with the source of one of them.
func TestLightCensusRanksDestinations(t *testing.T) {
	peers, l := newPeers(t,
		[2]string{"10.0.0.2:40001", "1.2.3.4:443"},
		[2]string{"10.0.0.2:40002", "1.2.3.4:443"},
		[2]string{"10.0.0.2:40003", "5.6.7.8:80"})
	s, ev := newTableStack(t, l, 0)
	for i, p := range peers {
		p.connect(nil)
		expect(t, ev.accepted, uint16(40001+i), "connection")
	}
	c := s.census(1)
	if c.TCP != 3 || len(c.Top) != 1 {
		t.Fatalf("census = %d open with %d top destinations; want 3 and 1", c.TCP, len(c.Top))
	}
	if top := c.Top[0]; top.Dst.NetAddr() != "1.2.3.4:443" || top.Count != 2 || top.Src.Address.String() != "10.0.0.2" {
		t.Fatalf("top destination = %s with %d from %s; want 1.2.3.4:443 with 2 from 10.0.0.2", top.Dst.NetAddr(), top.Count, top.Src.NetAddr())
	}
}

// A connection whose handler is done leaves the table.
func TestLightEndedConnectionLeavesTheTable(t *testing.T) {
	peers, l := newPeers(t, [2]string{"10.0.0.2:40001", "1.2.3.4:443"})
	s := &lightStack{ctx: t.Context(), tun: l, mtu: testMTU, window: tcpWindow, conns: make(map[flow]*tcpConn), tcp: newTCPTable(0)}
	done := make(chan struct{})
	s.handle = s.tracked(func(_ context.Context, conn net.Conn, _ net.Destination) {
		conn.Close()
		close(done)
	})
	s.udp = newUdpConnectionHandler(func(conn net.Conn, _ net.Destination) { conn.Close() }, s.writeUDP, time.Minute)
	s.Start()
	t.Cleanup(func() {
		s.Close()
		l.Close()
	})
	peers[0].connect(nil)
	<-done
	for deadline := time.Now().Add(5 * time.Second); s.census(0).TCP != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a connection whose handler returned is still in the table")
		}
	}
}
