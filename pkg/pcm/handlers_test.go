package pcm

import (
	"bytes"
	"testing"

	"github.com/pion/rtp"
)

func TestLowLatencyG711PreservesSamplesAndOrdered20msPackets(t *testing.T) {
	var got []*rtp.Packet
	send := RepackG711LowLatency(func(p *rtp.Packet) { got = append(got, p) })
	original := make([]byte, 1600)
	for i := range original {
		original[i] = byte(i)
	}
	offset := 0
	for _, size := range []int{1, 319, 77, 403, 640, 160} {
		send(&rtp.Packet{Header: rtp.Header{SSRC: 42, PayloadType: 8}, Payload: original[offset : offset+size]})
		offset += size
	}
	if len(got) != 10 {
		t.Fatalf("packets=%d", len(got))
	}
	var joined []byte
	for i, p := range got {
		if len(p.Payload) != 160 || p.Timestamp != uint32(i*160) || p.SequenceNumber != uint16(i) || p.SSRC != 42 || p.PayloadType != 8 || p.Marker != (i == 0) {
			t.Fatalf("packet%d: %+v bytes%d", i, p.Header, len(p.Payload))
		}
		joined = append(joined, p.Payload...)
	}
	if !bytes.Equal(original, joined) {
		t.Fatal("sample loss/reordering")
	}
	// Mutating the caller's next input cannot overwrite a delivered packet.
	before := append([]byte(nil), got[0].Payload...)
	send(&rtp.Packet{Payload: make([]byte, 640)})
	if !bytes.Equal(got[0].Payload, before) {
		t.Fatal("delivered payload aliases reused buffer")
	}
}

func TestLegacyG711Retains1024SamplesAndZeroTimestampOption(t *testing.T) {
	for _, zero := range []bool{false, true} {
		var got []*rtp.Packet
		send := RepackG711(zero, func(p *rtp.Packet) { got = append(got, p) })
		for i := 0; i < 7; i++ {
			send(&rtp.Packet{Payload: bytes.Repeat([]byte{byte(i)}, 320)})
		}
		if len(got) != 2 {
			t.Fatalf("packets=%d", len(got))
		}
		for i, p := range got {
			want := uint32(i * 1024)
			if zero {
				want = 0
			}
			if len(p.Payload) != 1024 || p.Timestamp != want || !p.Marker {
				t.Fatal("legacy framing changed")
			}
		}
	}
}
