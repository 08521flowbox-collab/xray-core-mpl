//go:build darwin

package tun

import (
	"encoding/binary"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/platform"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type countingDispatcher struct {
	delivered chan tcpip.NetworkProtocolNumber
}

func (d *countingDispatcher) DeliverNetworkPacket(protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	d.delivered <- protocol
}

func (d *countingDispatcher) DeliverLinkPacket(tcpip.NetworkProtocolNumber, *stack.PacketBuffer) {}

func ipv4Packet(payloadLen int) []byte {
	ip := make([]byte, 20+payloadLen)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	ip[8] = 64
	ip[9] = 17
	copy(ip[12:], []byte{172, 19, 0, 1})
	copy(ip[16:], []byte{1, 1, 1, 1})
	return append([]byte{0, 0, 0, unix.AF_INET}, ip...)
}

func injectedTun(t *testing.T) (*DarwinTun, int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(platform.TunFdKey, strconv.Itoa(fds[0]))
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	tun, err := NewTun(TunOptions{Name: "utun9", MTU: 1500})
	if err != nil {
		t.Fatal(err)
	}
	d := tun.(*DarwinTun)
	if d.ownsFd {
		t.Fatal("injected descriptor must not be owned")
	}
	if d.fd != fds[0] {
		t.Fatalf("fd = %d, want %d", d.fd, fds[0])
	}
	return d, fds[1]
}

func TestDarwinTunReadsInjectedDescriptor(t *testing.T) {
	tun, peer := injectedTun(t)
	pkt := ipv4Packet(8)
	if _, err := unix.Write(peer, pkt); err != nil {
		t.Fatal(err)
	}
	version, pb, err := tun.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	defer pb.DecRef()
	if version != 4 {
		t.Fatalf("version = %d, want 4", version)
	}
	if got := pb.Size(); got != len(pkt)-utunHeaderSize {
		t.Fatalf("packet size = %d, want %d", got, len(pkt)-utunHeaderSize)
	}
	if _, _, err := tun.ReadPacket(); err != ErrQueueEmpty {
		t.Fatalf("empty read err = %v, want ErrQueueEmpty", err)
	}
}

func TestDarwinTunWritePacketAddsHeader(t *testing.T) {
	tun, peer := injectedTun(t)
	payload := ipv4Packet(4)[utunHeaderSize:]
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(payload)})
	defer pkt.DecRef()
	if err := tun.WritePacket(pkt); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 2048)
	n, err := unix.Read(peer, out)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(payload)+utunHeaderSize || out[3] != unix.AF_INET {
		t.Fatalf("wrote %d bytes with family %d", n, out[3])
	}
}

func TestDarwinEndpointDetachWaitsForDispatchLoop(t *testing.T) {
	tun, peer := injectedTun(t)
	ep, err := tun.newEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	dispatcher := &countingDispatcher{delivered: make(chan tcpip.NetworkProtocolNumber, 1)}
	ep.Attach(dispatcher)
	if _, err := unix.Write(peer, ipv4Packet(8)); err != nil {
		t.Fatal(err)
	}
	select {
	case proto := <-dispatcher.delivered:
		if proto != 0x0800 {
			t.Fatalf("protocol = %#x", proto)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch loop delivered nothing")
	}

	start := time.Now()
	ep.Attach(nil)
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Attach(nil) took %v, want < 200ms", elapsed)
	}
	if ep.IsAttached() {
		t.Fatal("still attached")
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before {
		t.Fatalf("goroutines = %d, want <= %d", got, before)
	}
	t.Logf("Attach(nil) returned in %v", elapsed)
}

func TestDarwinTunDrainsABatchAndSkipsHeaderOnlyDatagrams(t *testing.T) {
	tun, peer := injectedTun(t)
	sizes := []int{8, 40, 1200}
	for _, size := range sizes {
		if _, err := unix.Write(peer, ipv4Packet(size)); err != nil {
			t.Fatal(err)
		}
		if _, err := unix.Write(peer, []byte{0, 0, 0, unix.AF_INET}); err != nil {
			t.Fatal(err)
		}
	}
	for i, size := range sizes {
		version, pb, err := tun.ReadPacket()
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if version != 4 || pb.Size() != 20+size {
			t.Fatalf("packet %d: version %d size %d, want 4/%d", i, version, pb.Size(), 20+size)
		}
		pb.DecRef()
	}
	if tun.rx.n != 2*len(sizes) {
		t.Fatalf("batch held %d datagrams, want %d in one recvmsg_x", tun.rx.n, 2*len(sizes))
	}
	if _, _, err := tun.ReadPacket(); err != ErrQueueEmpty {
		t.Fatalf("empty read err = %v, want ErrQueueEmpty", err)
	}
	if _, err := unix.Write(peer, ipv4Packet(16)); err != nil {
		t.Fatal(err)
	}
	if _, pb, err := tun.ReadPacket(); err != nil || pb.Size() != 36 {
		t.Fatalf("after refill: size %d err %v", pb.Size(), err)
	} else {
		pb.DecRef()
	}
}

func TestDarwinTunPayloadOutlivesTheSlot(t *testing.T) {
	tun, peer := injectedTun(t)
	first := ipv4Packet(8)
	first[len(first)-1] = 0xAA
	if _, err := unix.Write(peer, first); err != nil {
		t.Fatal(err)
	}
	_, pb, err := tun.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	defer pb.DecRef()
	second := ipv4Packet(8)
	second[len(second)-1] = 0xBB
	if _, err := unix.Write(peer, second); err != nil {
		t.Fatal(err)
	}
	if _, pb2, err := tun.ReadPacket(); err != nil {
		t.Fatal(err)
	} else {
		pb2.DecRef()
	}
	got := pb.ToView().AsSlice()
	if got[len(got)-1] != 0xAA {
		t.Fatalf("first packet's last byte became %#x after the slot was reused", got[len(got)-1])
	}
}
