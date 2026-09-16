package tun

import (
	"context"
	"net"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
)

type closedConn struct {
	closed bool
}

func (c *closedConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (c *closedConn) Write([]byte) (int, error)        { return 0, net.ErrClosed }
func (c *closedConn) Close() error                     { c.closed = true; return nil }
func (c *closedConn) LocalAddr() net.Addr              { return nil }
func (c *closedConn) RemoteAddr() net.Addr             { return nil }
func (c *closedConn) SetDeadline(time.Time) error      { return nil }
func (c *closedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *closedConn) SetWriteDeadline(time.Time) error { return nil }

func TestHandleConnectionDropsNilRemoteAddr(t *testing.T) {
	handler := &Handler{ctx: context.Background(), tag: "tun"}
	conn := &closedConn{}

	handler.HandleConnection(conn, xnet.TCPDestination(xnet.LocalHostIP, 5222))

	if !conn.closed {
		t.Fatal("a dropped connection must still be closed")
	}
}
