package bittorrent

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/xtls/xray-core/common"
)

type SniffHeader struct{}

func (h *SniffHeader) Protocol() string {
	return "bittorrent"
}

func (h *SniffHeader) Domain() string {
	return ""
}

var errNotBittorrent = errors.New("not bittorrent header")

func SniffBittorrent(b []byte) (*SniffHeader, error) {
	if len(b) < 20 {
		return nil, common.ErrNoClue
	}

	if b[0] == 19 && string(b[1:20]) == "BitTorrent protocol" {
		return &SniffHeader{}, nil
	}

	return nil, errNotBittorrent
}

func SniffUTP(b []byte) (*SniffHeader, error) {
	if len(b) < 20 {
		return nil, common.ErrNoClue
	}

	// type 4 (ST_SYN), version 1
	if b[0] != 0x41 {
		return nil, errNotBittorrent
	}

	// timestamp_difference is always 0 in new connections
	if binary.BigEndian.Uint32(b[8:12]) != 0 {
		return nil, errNotBittorrent
	}

	// Walk the extension chain. Selective ack (1) and extension bits (2)
	extension, offset := b[1], 20
	for extension != 0 {
		if len(b) < offset+2 {
			return nil, errNotBittorrent
		}
		length := int(b[offset+1])
		switch extension {
		case 1: // selective ack
			if length < 4 || length%4 != 0 {
				return nil, errNotBittorrent
			}
		case 2: // extension bits: fixed 8 bytes, sent in ST_SYN by µTorrent
			if length != 8 {
				return nil, errNotBittorrent
			}
		default:
			return nil, errNotBittorrent
		}
		if len(b) < offset+2+length {
			return nil, errNotBittorrent
		}
		extension = b[offset]
		offset += 2 + length
	}

	// extensions should consume all ST_SYN payload
	if len(b) != offset {
		return nil, errNotBittorrent
	}

	return &SniffHeader{}, nil
}

// udpTrackerProtocolID is the magic constant opening every BEP 15 connect request.
const udpTrackerProtocolID = 0x41727101980

// SniffUDPTracker matches the BEP 15 connect request, the first packet of every
// UDP tracker exchange.
func SniffUDPTracker(b []byte) (*SniffHeader, error) {
	if len(b) != 16 || binary.BigEndian.Uint64(b[:8]) != udpTrackerProtocolID || binary.BigEndian.Uint32(b[8:12]) != 0 {
		return nil, errNotBittorrent
	}
	return &SniffHeader{}, nil
}

// SniffDHT matches a BEP 5 KRPC message: a bencoded dictionary carrying a
// message type key and the sender's 20-byte node id.
func SniffDHT(b []byte) (*SniffHeader, error) {
	if len(b) < 2 || b[0] != 'd' || b[len(b)-1] != 'e' {
		return nil, errNotBittorrent
	}
	if !bytes.Contains(b, []byte("1:y1:")) || !bytes.Contains(b, []byte("2:id20:")) {
		return nil, errNotBittorrent
	}
	return &SniffHeader{}, nil
}

// SniffHTTPAnnounce matches a BEP 3 HTTP tracker announce or scrape. It has to
// run before the HTTP sniffer, which would otherwise claim the request.
func SniffHTTPAnnounce(b []byte) (*SniffHeader, error) {
	if len(b) < 4 {
		return nil, common.ErrNoClue
	}
	if !bytes.HasPrefix(b, []byte("GET ")) {
		return nil, errNotBittorrent
	}
	line, _, _ := bytes.Cut(b, []byte("\r\n"))
	if !bytes.Contains(line, []byte("info_hash=")) {
		return nil, errNotBittorrent
	}
	return &SniffHeader{}, nil
}
