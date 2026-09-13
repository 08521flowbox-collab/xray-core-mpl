package internet

import (
	"context"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
)

type panickingSystemDialer struct{}

func (panickingSystemDialer) Dial(context.Context, net.Address, net.Destination, *SocketConfig) (net.Conn, error) {
	panic("boom")
}

func (panickingSystemDialer) DestIpAddress() net.IP { return nil }

func TestTcpRaceDialSurvivesAPanickingDialer(t *testing.T) {
	previous := effectiveSystemDialer
	UseAlternativeSystemDialer(panickingSystemDialer{})
	t.Cleanup(func() { UseAlternativeSystemDialer(previous) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sockopt := &SocketConfig{HappyEyeballs: &HappyEyeballsConfig{MaxConcurrentTry: 2}}
	ips := []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.2")}

	done := make(chan error, 1)
	go func() {
		conn, err := TcpRaceDial(ctx, nil, ips, 443, sockopt, "example.invalid")
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a dialer that panics")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TcpRaceDial hung after the dial goroutine panicked")
	}
}
