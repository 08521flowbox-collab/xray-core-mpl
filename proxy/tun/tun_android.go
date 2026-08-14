//go:build android

package tun

import (
	"context"
	"strconv"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/platform"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type AndroidTun struct {
	tunFd   int
	options TunOptions
}

// DefaultTun implements Tun
var _ Tun = (*AndroidTun)(nil)

// DefaultTun implements GVisorTun
var _ GVisorTun = (*AndroidTun)(nil)

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

func (t *AndroidTun) newEndpoint() (stack.LinkEndpoint, error) {
	return fdbased.New(&fdbased.Options{
		FDs:               []int{t.tunFd},
		MTU:               t.options.MTU,
		RXChecksumOffload: true,
	})
}
