//go:build android

package tun

import (
	"bytes"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/platform"
	"golang.org/x/sys/unix"
)

// The stack reads and writes the tun through a descriptor of its own, which it closes before Close returns:
// the caller's stays open, and nobody reads it after that.
func TestStackLetsTheDescriptorGo(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	t.Setenv(platform.TunFdKey, strconv.Itoa(fds[0]))
	tun, err := NewTun(TunOptions{MTU: testMTU})
	if err != nil {
		t.Fatal(err)
	}
	stack, err := NewStack(t.Context(), StackOptions{Tun: tun, MTU: testMTU, IdleTimeout: time.Minute}, &Handler{ctx: t.Context(), config: &Config{}})
	if err != nil {
		t.Fatal(err)
	}
	stack.Start()

	src, dst := netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("1.1.1.1")
	if _, err := unix.Write(fds[1], ping(src, dst)); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2048)
	unix.SetsockoptTimeval(fds[1], unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 5})
	n, err := unix.Read(fds[1], reply)
	if err != nil {
		t.Fatal("no echo reply through the stack's descriptor: ", err)
	}
	if p, ok := parsePacket(reply[:n]); !ok || p.proto != protoICMPv4 || p.src != dst || p.dst != src || p.payload[0] != 0 {
		t.Fatal("not the echo reply: ", reply[:n])
	}

	closed := make(chan error, 1)
	go func() { closed <- stack.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while the stack was waiting for a packet")
	}

	if _, err := unix.FcntlInt(uintptr(fds[0]), unix.F_GETFD, 0); err != nil {
		t.Fatal("the caller's descriptor was closed: ", err)
	}
	later := ping(src, dst)
	if _, err := unix.Write(fds[1], later); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 2048)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		n, err = unix.Read(fds[0], got)
		if err != unix.EAGAIN || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || !bytes.Equal(got[:n], later) {
		t.Fatal("a packet after Close was not left to the caller: ", got[:n], err)
	}
}
