package tun

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/platform"
)

// packetRoom is what a tun may need in front of a packet in the buffer that it is read into
const packetRoom = 16

// packetTun is a tun that lightStack reads and writes a packet at a time
type packetTun interface {
	// ReadPacket waits for a packet, reads it into b and returns it
	ReadPacket(b []byte) ([]byte, error)
	// WritePacket writes a packet that is in two parts, the second of which may be empty
	WritePacket(header, payload []byte) error
	// Close ends ReadPacket
	Close() error
}

// segmenter is a tun whose other side takes TCP segments that are larger than the MTU,
// splits them only if it has to send them on, and completes their checksum then
type segmenter interface {
	// Segments reports whether WriteSegment can be used
	Segments() bool
	// WriteSegment writes a packet with a TCP segment whose payload is in parts, to be sent on with mss bytes
	// of it at most in one. Its checksum is that of the pseudo-header only, not inverted
	WriteSegment(header []byte, payload [][]byte, mss int) error
}

// flow is what the packets of one connection have in common, src is the side of the system
type flow struct {
	src, dst netip.AddrPort
}

// lightStack turns the packets that the system sends to the tun into connections and back.
// It has only what the two need to talk through a tun, and a local link when the system passes on
// what others send, see tcpConn. The tun may also be a tunnel to a system that is far away, and the stack
// the one that opens the connections
type lightStack struct {
	ctx context.Context
	tun packetTun
	mtu int
	// segmenter is the tun if it takes segments that are larger than its MTU
	segmenter segmenter
	// window is the window of every connection
	window int32
	// verify is set if the checksums of what comes are looked at
	verify bool
	// stamps is set if connections carry the times of RFC 7323 when the other side does
	stamps bool
	// handle is given every new connection that the other side opens
	handle func(conn net.Conn, destination net.Destination)
	udp    *udpConnectionHandler
	tcp    *tcpTable
	// addresses are what connections are opened from
	addresses []netip.Addr

	mu      sync.Mutex
	conns   map[flow]*tcpConn
	closed  bool
	reading sync.WaitGroup
	// opening is how many of conns wait for the answer to their SYN or SYN-ACK
	opening atomic.Int32
	// fragments are the packets that are larger than a link on their way and come in parts.
	// identification numbers the ones that leave like that: a counter for each of a few groups of destinations,
	// each from a start of its own, so that no two of them that go one way meet, and no destination learns how much
	// goes to the others
	fragments      reassembly
	identification [64]atomic.Uint32
}

func newLightStack(ctx context.Context, tun packetTun, mtu uint32, idleTimeout time.Duration, handler *Handler) *lightStack {
	s := &lightStack{
		ctx:    ctx,
		tun:    tun,
		mtu:    int(mtu),
		window: tcpWindow,
		conns:  make(map[flow]*tcpConn),
	}
	for i := range s.identification {
		s.identification[i].Store(rand.Uint32())
	}
	maxTCPConns := platform.NewEnvFlag(platform.TunMaxTCPConnsKey).GetValueAsInt(0)
	s.tcp = newTCPTable(maxTCPConns)
	s.handle = s.tracked(handler.handleConnection)
	s.udp = newUdpConnectionHandler(handler.HandleConnection, s.writeUDP, idleTimeout)
	s.udp.hijackDNS = handler.dnsHijack(s.writeUDP)
	errors.LogInfo(ctx, "tun: built-in tcp/ip stack, mtu ", mtu, ", window ", s.window, ", max tcp connections ", maxTCPConns)
	return s
}

// tracked hands handle the connections in the table, each with a context that ends when the table lets it go
func (s *lightStack) tracked(handle func(context.Context, net.Conn, net.Destination)) func(net.Conn, net.Destination) {
	return func(conn net.Conn, dst net.Destination) {
		c := conn.(*tcpConn)
		ctx, cancel := context.WithCancel(s.ctx)
		e := &tcpEntry{src: c.src, dst: dst, cancel: cancel, close: c.Close}
		s.tcp.add(e)
		handle(ctx, &liveConn{tcpConn: c, entry: e}, dst)
		s.tcp.remove(e)
		cancel()
	}
}

func (s *lightStack) census(top int) Census {
	var c Census
	c.TCP, c.Top = s.tcp.census(top)
	c.TCPEvicted = s.tcp.evictions.Load()
	c.UDP, c.UDPEvicted = s.udp.census()
	return c
}

// Start is called by Handler to bring stack to life
func (s *lightStack) Start() error {
	s.reading.Add(1)
	go func() {
		defer s.reading.Done()
		s.read()
	}()
	return nil
}

// Close resets the connections, stops reading the tun and lets the UDP flows go. Nothing is read after it returns,
// so no flow comes after the ones that it lets go
func (s *lightStack) Close() error {
	s.mu.Lock()
	s.closed = true
	conns := make([]*tcpConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		if c.err == nil {
			c.reset()
		}
		c.mu.Unlock()
	}
	err := s.tun.Close()
	s.reading.Wait()
	s.udp.Close()
	return err
}

// read takes the packets of the tun until it is closed
func (s *lightStack) read() {
	// a tun that does not split segments gives them as large as a packet can be
	b := make([]byte, packetRoom+1<<16)
	for {
		b, err := s.tun.ReadPacket(b)
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if !closed {
				errors.LogErrorInner(s.ctx, err, "[tun] failed to read a packet, no more are read")
			}
			return
		}
		s.Input(b)
	}
}

// Input takes a packet of the tun
func (s *lightStack) Input(b []byte) {
	p, ok := parsePacket(b)
	if !ok || p.src.Is4In6() || p.dst.Is4In6() || p.src.IsLoopback() || p.dst.IsLoopback() || p.dst.IsUnspecified() || p.src.IsMulticast() {
		// nothing comes from many at once, and an address that stands for this machine itself is not one
		// that the other side of a tun can ask for. Nor is an IPv6 address that stands for an IPv4 one, which
		// would be dialed as that: ::ffff:0.0.0.0 is this machine as well
		return
	}
	if s.verify && b[0]>>4 == 4 && checksum(sum(b[:int(b[0]&0xf)*4], 0)) != 0 {
		return
	}
	// what was put together may be of two packets with one number, its checksum tells
	verify := s.verify || p.fragment
	if p.fragment {
		p, ok = s.fragments.add(p)
	}
	if !ok || verify && !p.verified() {
		return
	}
	switch p.proto {
	case protoTCP:
		s.handleTCP(p)
	case protoUDP:
		s.handleUDP(p)
	case protoICMPv4, protoICMPv6:
		s.handleICMP(p)
	}
}

// local returns the address that packets to remote are sent from
func (s *lightStack) local(remote netip.Addr) (netip.Addr, error) {
	for _, a := range s.addresses {
		if a.Is4() == remote.Is4() {
			return a, nil
		}
	}
	return netip.Addr{}, errors.New("no address to reach ", remote, " from")
}

// ephemeral returns a port for what is opened from here: the first that free takes, from a random one on
func ephemeral(free func(port uint16) bool) (uint16, bool) {
	start := rand.Uint32()
	for i := range uint32(1 << 15) {
		if port := uint16(1<<15 | (start+i)&(1<<15-1)); free(port) {
			return port, true
		}
	}
	return 0, false
}

func (s *lightStack) handleTCP(p packet) {
	t := p.payload
	if len(t) < tcpHeaderSize {
		return
	}
	size := int(t[12]>>4) * 4
	if size < tcpHeaderSize || size > len(t) {
		return
	}
	f := flow{
		src: netip.AddrPortFrom(p.src, binary.BigEndian.Uint16(t)),
		dst: netip.AddrPortFrom(p.dst, binary.BigEndian.Uint16(t[2:])),
	}
	seg := segment{
		seq:     binary.BigEndian.Uint32(t[4:]),
		ack:     binary.BigEndian.Uint32(t[8:]),
		flags:   t[13],
		window:  binary.BigEndian.Uint16(t[14:]),
		options: t[tcpHeaderSize:size],
		payload: t[size:],
	}

	s.mu.Lock()
	c := s.conns[f]
	s.mu.Unlock()
	if c != nil && c.input(seg) {
		return
	}
	if seg.flags&(tcpSyn|tcpAck|tcpRst) != tcpSyn || s.handle == nil {
		if seg.flags&tcpRst == 0 {
			s.refuse(f, seg)
		}
		return
	}

	if s.opening.Load() >= tcpOpening {
		return
	}
	c = newTCPConn(s, f)
	c.synchronize(seg)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.conns[f] = c
	s.mu.Unlock()
	s.opening.Add(1)
	c.mu.Lock()
	c.sendSyn()
	c.arm(tcpTick)
	c.mu.Unlock()
}

// refuse answers a segment of a connection that is not known with a reset
func (s *lightStack) refuse(f flow, seg segment) {
	var b [ipv6HeaderSize + tcpHeaderSize]byte
	n := putIPHeader(b[:], protoTCP, f.dst.Addr(), f.src.Addr())
	t := b[n : n+tcpHeaderSize]
	binary.BigEndian.PutUint16(t, f.dst.Port())
	binary.BigEndian.PutUint16(t[2:], f.src.Port())
	t[12] = tcpHeaderSize / 4 << 4
	if seg.flags&tcpAck != 0 {
		binary.BigEndian.PutUint32(t[4:], seg.ack)
		t[13] = tcpRst
	} else {
		ack := seg.seq + uint32(len(seg.payload))
		if seg.flags&(tcpSyn|tcpFin) != 0 {
			ack++
		}
		binary.BigEndian.PutUint32(t[8:], ack)
		t[13] = tcpRst | tcpAck
	}
	setIPLength(b[:n], n+tcpHeaderSize)
	binary.BigEndian.PutUint16(t[16:], transportChecksum(b[:n], protoTCP, t, nil))
	_ = s.tun.WritePacket(b[:n+tcpHeaderSize], nil)
}

// remove forgets a connection. c.mu is held
func (s *lightStack) remove(c *tcpConn) {
	if !c.established {
		s.opening.Add(-1)
	}
	s.mu.Lock()
	if s.conns[c.flow] == c {
		delete(s.conns, c.flow)
	}
	s.mu.Unlock()
}

// handleUDP hands a datagram to the flow of its source. The read buffer is used again, the flow gets a copy
func (s *lightStack) handleUDP(p packet) {
	u := p.payload
	if len(u) < udpHeaderSize {
		return
	}
	size := int(binary.BigEndian.Uint16(u[4:]))
	if size < udpHeaderSize || size > len(u) {
		return
	}
	src := net.UDPDestination(net.IPAddress(p.src.AsSlice()), net.Port(binary.BigEndian.Uint16(u)))
	dst := net.UDPDestination(net.IPAddress(p.dst.AsSlice()), net.Port(binary.BigEndian.Uint16(u[2:])))
	s.udp.HandlePacket(src, dst, bytes.Clone(u[udpHeaderSize:size]))
}

// writeUDP writes a packet with the payload from src to dst to the tun
func (s *lightStack) writeUDP(payload []byte, src net.Destination, dst net.Destination) error {
	srcIP, _ := netip.AddrFromSlice(src.Address.IP())
	dstIP, _ := netip.AddrFromSlice(dst.Address.IP())
	return s.sendUDP(payload, netip.AddrPortFrom(srcIP, uint16(src.Port)), netip.AddrPortFrom(dstIP, uint16(dst.Port)))
}

func (s *lightStack) sendUDP(payload []byte, src, dst netip.AddrPort) error {
	srcIP, dstIP := src.Addr().Unmap(), dst.Addr().Unmap()
	if !srcIP.IsValid() || srcIP.Is4() != dstIP.Is4() || len(payload) > math.MaxUint16-udpHeaderSize {
		return errors.New("failed to write udp packet of ", len(payload), " bytes from ", src, " to ", dst)
	}
	var b [ipv6HeaderSize + udpHeaderSize]byte
	n := putIPHeader(b[:], protoUDP, srcIP, dstIP)
	u := b[n : n+udpHeaderSize]
	binary.BigEndian.PutUint16(u, src.Port())
	binary.BigEndian.PutUint16(u[2:], dst.Port())
	binary.BigEndian.PutUint16(u[4:], uint16(udpHeaderSize+len(payload)))
	check := transportChecksum(b[:n], protoUDP, u, payload)
	if check == 0 {
		// zero stands for no checksum
		check = 0xffff
	}
	binary.BigEndian.PutUint16(u[6:], check)
	return s.write(b[:n+udpHeaderSize], payload)
}

// write completes the IP header of a packet that is in two parts and writes it to the tun, in fragments if it is
// larger than the MTU. A segment of a connection never is, and is not written by this
func (s *lightStack) write(header, payload []byte) error {
	size, last := ipv6HeaderSize, 39
	if header[0]>>4 == 4 {
		size, last = ipv4HeaderSize, 19
	}
	// the group is chosen by the last bytes of the destination
	id := s.identification[(header[last]^header[last-1]<<3)%byte(len(s.identification))].Add(1)
	if header[0]>>4 == 4 {
		// a link on the way may split it as well, and what is put together there is told apart by its number
		binary.BigEndian.PutUint16(header[4:], uint16(id))
	}
	if len(header)+len(payload) > s.mtu {
		return writeFragments(s.tun, s.mtu, id, header, payload)
	}
	setIPLength(header[:size], len(header)+len(payload))
	return s.tun.WritePacket(header, payload)
}

// handleICMP answers an echo request as its destination would, and takes the word that a packet of a connection
// was too large for a link on its way or found no way at all
func (s *lightStack) handleICMP(p packet) {
	m := p.payload
	request, reply := byte(8), byte(0)
	if p.proto == protoICMPv6 {
		request, reply = 128, 129
	}
	if len(m) < 8 {
		return
	}
	switch {
	case p.proto == protoICMPv4 && m[0] == 3 && m[1] == 4:
		s.undelivered(m[8:], max(int(binary.BigEndian.Uint16(m[6:])), ipv4SmallestMTU))
		return
	case p.proto == protoICMPv6 && m[0] == 2:
		s.undelivered(m[8:], max(int(binary.BigEndian.Uint32(m[4:])), ipv6SmallestMTU))
		return
	case p.proto == protoICMPv4 && m[0] == 3, p.proto == protoICMPv6 && m[0] == 1:
		s.undelivered(m[8:], 0)
		return
	}
	if m[0] != request || m[1] != 0 || s.handle == nil && !slices.Contains(s.addresses, p.dst) {
		// a stack that takes no connections is one host, not all of them
		return
	}
	var b [ipv6HeaderSize]byte
	n := putIPHeader(b[:], p.proto, p.dst, p.src)
	// the reply is the request with another type
	m[0], m[2], m[3] = reply, 0, 0
	if p.proto == protoICMPv6 {
		binary.BigEndian.PutUint16(m[2:], transportChecksum(b[:n], protoICMPv6, m, nil))
	} else {
		binary.BigEndian.PutUint16(m[2:], checksum(sum(m, 0)))
	}
	_ = s.write(b[:n], m)
}

// undelivered takes the start of a packet that was sent from here and did not get to where it goes. If it did not fit
// a link on its way, which takes packets of mtu bytes, the connection that sent it goes on with smaller segments.
// Without an mtu there is no way at all, and a connection that is being opened is not waited for
func (s *lightStack) undelivered(b []byte, mtu int) {
	var proto byte
	var local, remote netip.Addr
	switch {
	case len(b) >= ipv4HeaderSize && b[0]>>4 == 4:
		proto, local, remote = b[9], netip.AddrFrom4([4]byte(b[12:16])), netip.AddrFrom4([4]byte(b[16:20]))
		b = b[min(int(b[0]&0xf)*4, len(b)):]
	case len(b) >= ipv6HeaderSize && b[0]>>4 == 6:
		proto, local, remote = b[6], netip.AddrFrom16([16]byte(b[8:24])), netip.AddrFrom16([16]byte(b[24:40]))
		b = b[ipv6HeaderSize:]
	}
	// of the segment there are the ports and the sequence number at least
	if proto != protoTCP || len(b) < 8 {
		return
	}
	s.mu.Lock()
	c := s.conns[flow{
		src: netip.AddrPortFrom(remote, binary.BigEndian.Uint16(b[2:])),
		dst: netip.AddrPortFrom(local, binary.BigEndian.Uint16(b)),
	}]
	s.mu.Unlock()
	if c == nil {
		return
	}
	if seq := binary.BigEndian.Uint32(b[4:]); mtu != 0 {
		c.shrink(seq, mtu)
	} else {
		c.unreachable(seq)
	}
}

// liveConn is a connection of the table: what is read from it and written to it keeps it from being the quietest
type liveConn struct {
	*tcpConn
	entry *tcpEntry
}

func (c *liveConn) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := c.tcpConn.ReadMultiBuffer()
	if !mb.IsEmpty() {
		c.entry.touch()
	}
	return mb, err
}

func (c *liveConn) Read(p []byte) (int, error) {
	n, err := c.tcpConn.Read(p)
	if n > 0 {
		c.entry.touch()
	}
	return n, err
}

func (c *liveConn) WriteMultiBuffer(mb buf.MultiBuffer) error {
	c.entry.touch()
	return c.tcpConn.WriteMultiBuffer(mb)
}

func (c *liveConn) Write(p []byte) (int, error) {
	c.entry.touch()
	return c.tcpConn.Write(p)
}
