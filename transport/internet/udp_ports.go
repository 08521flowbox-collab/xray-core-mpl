package internet

import (
	"sync"

	"github.com/xtls/xray-core/common/net"
)

var ownUDPPorts sync.Map

func noteOwnUDPPort(conn net.PacketConn) {
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		ownUDPPorts.Store(net.Port(addr.Port), conn)
	}
}

func forgetOwnUDPPort(conn net.PacketConn) {
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		ownUDPPorts.CompareAndDelete(net.Port(addr.Port), conn)
	}
}

func IsOwnUDPPort(port net.Port) bool {
	_, found := ownUDPPorts.Load(port)
	return found
}

func udpListenAddr(src net.Addr, dest *net.UDPAddr) (string, net.Addr) {
	if src != nil {
		return "udp", src
	}
	if dest.IP.To4() != nil {
		return "udp4", &net.UDPAddr{IP: []byte{0, 0, 0, 0}}
	}
	return "udp6", &net.UDPAddr{IP: make([]byte, 16)}
}
