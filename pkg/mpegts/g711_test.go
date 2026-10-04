package mpegts

import (
	"bytes"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

func audioCodecs(c *Consumer) (names []string) {
	for _, media := range c.Medias {
		if media.Kind == core.KindAudio {
			for _, codec := range media.Codecs {
				names = append(names, codec.Name)
			}
		}
	}
	return
}

// A plain stream.ts consumer must stay exactly as upstream: AAC only.
func TestConsumerDefaultAudioIsAACOnly(t *testing.T) {
	got := audioCodecs(NewConsumer())
	if len(got) != 1 || got[0] != core.CodecAAC {
		t.Fatalf("default audio codecs = %v, want [AAC]", got)
	}
}

func TestConsumerWithG711(t *testing.T) {
	c := NewConsumer()
	c.WithG711(true, false)
	got := audioCodecs(c)
	if len(got) != 2 || got[1] != core.CodecPCMU {
		t.Fatalf("audio codecs = %v, want [AAC PCMU]", got)
	}
	if other := audioCodecs(NewConsumer()); len(other) != 1 {
		t.Fatalf("WithG711 leaked into a new consumer: %v", other)
	}
}

// Muxes PCMU next to H.264 and reads it back with go2rtc's own demuxer.
func TestMuxPCMURoundTrip(t *testing.T) {
	m := NewMuxer()
	m.AddTrack(StreamTypeH264)
	pid := m.AddTrack(StreamTypePCMUTapo)

	ulaw := bytes.Repeat([]byte{0xff, 0x7f, 0x00, 0x80}, 40) // one 20 ms packet, 160 samples
	var ts bytes.Buffer
	ts.Write(m.GetHeader())
	ts.Write(m.GetPayload(pid, 1800, ulaw)) // 20 ms at 90 kHz
	ts.Write(m.GetPayload(pid, 3600, ulaw))

	d := NewDemuxer()
	var sawPMT, sawAudio bool
	for i := 0; i < 8; i++ {
		pkt, err := d.ReadPacket(&ts)
		if err != nil {
			break
		}
		switch pkt.PayloadType {
		case StreamTypeMetadata:
			sawPMT = bytes.Equal(pkt.Payload, []byte{StreamTypeH264, StreamTypePCMUTapo})
		case StreamTypePCMUTapo:
			if !bytes.Equal(pkt.Payload, ulaw) {
				t.Fatalf("payload %d bytes, want the 160 u-law bytes back", len(pkt.Payload))
			}
			sawAudio = true
		}
	}
	if !sawPMT || !sawAudio {
		t.Fatalf("PMT with [H264 PCMU]: %v, PCMU PES: %v", sawPMT, sawAudio)
	}
}
