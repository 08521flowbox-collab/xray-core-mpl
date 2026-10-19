//go:build android

package tun

import (
	"context"
	"strconv"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/platform"
	"golang.org/x/sys/unix"
)

type AndroidTun struct {
	tunFd   int
	options TunOptions
}

// DefaultTun implements Tun
var _ Tun = (*AndroidTun)(nil)

// NewTun builds new tun interface handler.
//
// The descriptor belongs to the caller — libzapcore's package comment states the
// contract the Kotlin side is built on: this only marks the fd non-blocking, it
// does not dup it, and AndroidTun.Close is empty. So no failure path here may
// close it. Closing a descriptor the caller still holds hands its number back to
// the process, and the next socket anything opens gets it; the caller then goes
// on using that number for the tunnel. That is the use-after-close the Kotlin
// teardown is written to avoid, entered through a door nobody was watching.
// Returning the error is enough — the owner decides what to do with the fd.
func NewTun(options TunOptions) (Tun, error) {
	fd, err := strconv.Atoi(platform.NewEnvFlag(platform.TunFdKey).GetValue(func() string { return "0" }))
	errors.LogInfo(context.Background(), "read Android Tun Fd ", fd, err)
	if err != nil {
		// Without this the fallback is fd 0 — stdin — and SetNonblock succeeds on
		// it, so the stack would attach to stdin instead of the tunnel.
		return nil, err
	}

	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, err
	}

	return &AndroidTun{
		tunFd:   fd,
		options: options,
	}, nil
}

func (t *AndroidTun) Start() error {
	return nil
}

func (t *AndroidTun) Close() error {
	return nil
}

// packets reads and writes the tun through a duplicate of the descriptor, the stack's own to close: closing it
// ends the reading and leaves the caller's descriptor alone
func (t *AndroidTun) packets() (packetTun, error) {
	fd, err := unix.FcntlInt(uintptr(t.tunFd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return fdPackets{newFdTun(fd, 0)}, nil
}

type fdPackets struct {
	*fdTun
}

func (p fdPackets) Close() error {
	return p.closeFile()
}
