package bittorrent

import (
	"encoding/binary"
	"testing"

	"github.com/xtls/xray-core/common"
)

// utpPacket builds the fixed 20-byte header defined by BEP 29.
func utpPacket(packetType, extension byte, tsDiff uint32, payload ...byte) []byte {
	b := make([]byte, 20)
	b[0] = packetType<<4 | 1
	b[1] = extension
	binary.BigEndian.PutUint16(b[2:4], 0x4a3f)     // connection_id, random
	binary.BigEndian.PutUint32(b[4:8], 0x8c3a91d2) // timestamp_microseconds, sender's clock
	binary.BigEndian.PutUint32(b[8:12], tsDiff)
	binary.BigEndian.PutUint32(b[12:16], 0x00100000) // wnd_size
	binary.BigEndian.PutUint16(b[16:18], 0x71ee)     // seq_nr, random in libutp/libtorrent
	binary.BigEndian.PutUint16(b[18:20], 0x0000)     // ack_nr
	return append(b, payload...)
}

func TestSniffUTP(t *testing.T) {
	selectiveAck := []byte{0, 4, 0xff, 0x00, 0xff, 0x00}
	wrongVersion := utpPacket(4, 0, 0)
	wrongVersion[0] = 4<<4 | 2

	cases := []struct {
		name    string
		payload []byte
		err     error
	}{
		{"syn", utpPacket(4, 0, 0), nil},
		{"syn with selective ack", append(utpPacket(4, 1, 0), selectiveAck...), nil},
		{"syn with extension bits", append(utpPacket(4, 2, 0), 0, 8, 1, 2, 3, 4, 5, 6, 7, 8), nil},
		{"extension bits with wrong length", append(utpPacket(4, 2, 0), 0, 4, 1, 2, 3, 4), errNotBittorrent},
		{"syn with nonzero timestamp_difference", utpPacket(4, 0, 0x1234), errNotBittorrent},
		{"syn with trailing payload", utpPacket(4, 0, 0, 'x'), errNotBittorrent},
		// txid 0x4100, no EDNS0: the worst case colliding with the uTP header
		{"dns query", []byte{
			0x41, 0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x01, 'a', 0x02, 'c', 'o', 0x00, 0x00, 0x01, 0x00, 0x01,
		}, errNotBittorrent},
		{"established connection packets", utpPacket(0, 0, 0x5678, 'x', 'y', 'z'), errNotBittorrent},
		{"state", utpPacket(2, 0, 0x5678), errNotBittorrent},
		{"fin", utpPacket(1, 0, 0x5678), errNotBittorrent},
		{"wrong version", wrongVersion, errNotBittorrent},
		{"unknown packet type", utpPacket(5, 0, 0), errNotBittorrent},
		{"unknown extension", utpPacket(4, 2, 0), errNotBittorrent},
		{"extension chain past the datagram", utpPacket(4, 1, 0, 0, 8, 0xff), errNotBittorrent},
		{"selective ack not in multiples of 4", append(utpPacket(4, 1, 0), 0, 3, 0xff, 0x00, 0xff), errNotBittorrent},
		{"shorter than the header", utpPacket(4, 0, 0)[:19], common.ErrNoClue},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, err := SniffUTP(c.payload)
			if err != c.err {
				t.Fatalf("expected error %v, got %v", c.err, err)
			}
			if err == nil && h == nil {
				t.Fatal("expected a sniff header, got nil")
			}
		})
	}
}

func TestSniffUDPTracker(t *testing.T) {
	connect := make([]byte, 16)
	binary.BigEndian.PutUint64(connect[:8], udpTrackerProtocolID)
	binary.BigEndian.PutUint32(connect[12:], 0x5a1f03c7)
	announce := append([]byte{}, connect...)
	binary.BigEndian.PutUint32(announce[8:12], 1)

	cases := []struct {
		name    string
		payload []byte
		err     error
	}{
		{"connect", connect, nil},
		{"announce action", announce, errNotBittorrent},
		{"wrong magic", make([]byte, 16), errNotBittorrent},
		{"trailing bytes", append(append([]byte{}, connect...), 0), errNotBittorrent},
		{"truncated", connect[:12], errNotBittorrent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := SniffUDPTracker(c.payload); err != c.err {
				t.Fatalf("expected error %v, got %v", c.err, err)
			}
		})
	}
}

func TestSniffDHT(t *testing.T) {
	id := "abcdefghij0123456789"
	cases := []struct {
		name    string
		payload string
		err     error
	}{
		{"ping query", "d1:ad2:id20:" + id + "e1:q4:ping1:t2:aa1:y1:qe", nil},
		{"get_peers query", "d1:ad2:id20:" + id + "9:info_hash20:" + id + "e1:q9:get_peers1:t2:aa1:v4:UT\x01\x011:y1:qe", nil},
		{"response with BEP 42 ip", "d2:ip6:\x01\x02\x03\x04\x1a\xe11:rd2:id20:" + id + "e1:t2:aa1:y1:re", nil},
		{"bencoded without node id", "d1:y1:q1:t2:aae", errNotBittorrent},
		{"node id outside a dictionary", "x1:y1:q2:id20:" + id + "e", errNotBittorrent},
		{"unterminated", "d1:ad2:id20:" + id + "e1:q4:ping1:t2:aa1:y1:q", errNotBittorrent},
		{"empty", "", errNotBittorrent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := SniffDHT([]byte(c.payload)); err != c.err {
				t.Fatalf("expected error %v, got %v", c.err, err)
			}
		})
	}
}

func TestSniffHTTPAnnounce(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		err     error
	}{
		{"announce", "GET /announce?info_hash=%8da%ab&peer_id=-qB4650-&port=13466 HTTP/1.1\r\nHost: tracker.example\r\n\r\n", nil},
		{"private tracker passkey path", "GET /a1b2c3/announce.php?info_hash=%8da%ab HTTP/1.1\r\n\r\n", nil},
		{"scrape", "GET /scrape?info_hash=%8da%ab HTTP/1.0\r\n\r\n", nil},
		{"ordinary get", "GET /index.html HTTP/1.1\r\nHost: example.com\r\n\r\n", errNotBittorrent},
		{"info_hash only in a header", "GET / HTTP/1.1\r\nReferer: /?info_hash=x\r\n\r\n", errNotBittorrent},
		{"post", "POST /announce?info_hash=x HTTP/1.1\r\n\r\n", errNotBittorrent},
		{"too short", "GE", common.ErrNoClue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := SniffHTTPAnnounce([]byte(c.payload)); err != c.err {
				t.Fatalf("expected error %v, got %v", c.err, err)
			}
		})
	}
}
