package tun

import (
	"context"
	"errors"

	xerrors "github.com/xtls/xray-core/common/errors"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

var ErrQueueEmpty = errors.New("queue is empty")

type GVisorDevice interface {
	WritePacket(packet *stack.PacketBuffer) tcpip.Error
	ReadPacket() (byte, *stack.PacketBuffer, error)
	Wait()
}

// LinkEndpoint implements GVisor stack.LinkEndpoint
var _ stack.LinkEndpoint = (*LinkEndpoint)(nil)

type LinkEndpoint struct {
	deviceMTU        uint32
	device           GVisorDevice
	dispatcherCancel context.CancelFunc
	dispatcherDone   chan struct{}
}

func (e *LinkEndpoint) MTU() uint32 {
	return e.deviceMTU
}

func (e *LinkEndpoint) SetMTU(_ uint32) {
	// not Implemented, as it is not expected GVisor will be asking tun device to be modified
}

func (e *LinkEndpoint) MaxHeaderLength() uint16 {
	return 0
}

func (e *LinkEndpoint) LinkAddress() tcpip.LinkAddress {
	return ""
}

func (e *LinkEndpoint) SetLinkAddress(_ tcpip.LinkAddress) {
	// not Implemented, as it is not expected GVisor will be asking tun device to be modified
}

func (e *LinkEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityRXChecksumOffload
}

// Attach starts the dispatch loop, or with a nil dispatcher stops it and
// returns only once the loop has left ReadPacket/Wait for good — the caller
// closes the descriptor next, and on Darwin that descriptor may not even be
// ours to close (see tun_darwin.go).
func (e *LinkEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.stopDispatcher()

	if dispatcher != nil {
		ctx, cancel := context.WithCancel(context.Background())
		e.dispatcherCancel = cancel
		e.dispatcherDone = make(chan struct{})
		go e.dispatchLoop(ctx, dispatcher, e.dispatcherDone)
	}
}

func (e *LinkEndpoint) stopDispatcher() {
	if e.dispatcherCancel == nil {
		return
	}
	e.dispatcherCancel()
	e.dispatcherCancel = nil
	<-e.dispatcherDone
	e.dispatcherDone = nil
}

func (e *LinkEndpoint) IsAttached() bool {
	return e.dispatcherCancel != nil
}

func (e *LinkEndpoint) Wait() {

}

func (e *LinkEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

func (e *LinkEndpoint) AddHeader(buffer *stack.PacketBuffer) {
	// tun interface doesn't have link layer header, it will be added by the OS
}

func (e *LinkEndpoint) ParseHeader(ptr *stack.PacketBuffer) bool {
	return true
}

func (e *LinkEndpoint) Close() {
	e.stopDispatcher()
}

func (e *LinkEndpoint) SetOnCloseAction(_ func()) {

}

func (e *LinkEndpoint) WritePackets(packetBufferList stack.PacketBufferList) (int, tcpip.Error) {
	var n int
	var err tcpip.Error

	for _, packetBuffer := range packetBufferList.AsSlice() {
		err = e.device.WritePacket(packetBuffer)
		if err != nil {
			return n, &tcpip.ErrAborted{}
		}
		n++
	}

	return n, nil
}

func (e *LinkEndpoint) dispatchLoop(ctx context.Context, dispatcher stack.NetworkDispatcher, done chan struct{}) {
	defer close(done)
	var networkProtocolNumber tcpip.NetworkProtocolNumber
	var version byte
	var packet *stack.PacketBuffer
	var err error

	for {
		select {
		case <-ctx.Done():
			return
		default:
			version, packet, err = e.device.ReadPacket()
			// on "queue empty", ask device to yield slightly and continue
			if errors.Is(err, ErrQueueEmpty) {
				e.device.Wait()
				continue
			}
			// stop dispatcher loop on any other interface failure; Attach(nil)
			// would wait for this very goroutine, so only log and leave
			if err != nil {
				xerrors.LogWarning(ctx, "tun dispatch loop stopped: ", err)
				return
			}

			// extract network protocol number from the packet first byte
			// (which is returned separately, since it is so incredibly hard to extract one byte from
			// stack.PacketBuffer without additional memory allocation and full copying it back and forth)
			switch version {
			case 4:
				networkProtocolNumber = header.IPv4ProtocolNumber
			case 6:
				networkProtocolNumber = header.IPv6ProtocolNumber
			default:
				// discard unknown network protocol packet
				packet.DecRef()
				continue
			}

			// dispatch the buffer to the stack
			dispatcher.DeliverNetworkPacket(networkProtocolNumber, packet)
			// signal the buffer that it can be released
			packet.DecRef()
		}
	}
}
