// Package control provides the socket control function type used by the
// system dialer and listener.
//
// Upstream Xray-core takes this type from github.com/sagernet/sing, which is
// licensed GPL-3.0-or-later. This fork drops that dependency, so the type is
// defined locally instead. It is a plain alias of the standard library's
// net.Dialer.Control / net.ListenConfig.Control signature, so anything that
// satisfied the original alias still satisfies this one.
package control

import "syscall"

// Func operates on a socket's file descriptor before it is put into use.
type Func = func(network, address string, conn syscall.RawConn) error
