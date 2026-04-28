package webrtc

import (
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type Track struct {
	kind     string
	id       string
	streamID string
	sequence uint16
	ssrc     uint32
	writer   webrtc.TrackLocalWriter
	mu       sync.Mutex

	// Audio-level extension state. Only populated for audio tracks where the
	// peer also negotiated the urn:ietf:params:rtp-hdrext:ssrc-audio-level
	// extension. audioLevelExtID is the negotiated 1-byte extension ID
	// (1..14); zero means "extension not present, skip".
	//
	// audioLevelByPT maps each negotiated audio PT to a per-codec computer.
	// go2rtc may pick any negotiated codec at runtime (no transcode if the
	// source already matches one of the offered codecs), so we have to
	// dispatch by the payload type that arrives at WriteRTP — not by the
	// "first" codec returned from Bind.
	audioLevelExtID uint8
	audioLevelByPT  map[uint8]*audioLevelComputer
}

func NewTrack(kind string) *Track {
	return &Track{
		kind:     kind,
		id:       "go2rtc-" + kind,
		streamID: "go2rtc",
	}
}

func (t *Track) Bind(context webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	t.mu.Lock()
	t.ssrc = uint32(context.SSRC())
	t.writer = context.WriteStream()

	// Pick the first negotiated codec — Track returns it to Pion below.
	var chosen webrtc.RTPCodecParameters
	for _, parameters := range context.CodecParameters() {
		chosen = parameters
		break
	}

	// If the peer negotiated the audio-level extension AND this is an audio
	// track, build a per-PT level-computer table. We can't know in advance
	// which codec WriteRTP will be called with, so prepare them all.
	if t.kind == "audio" {
		for _, ext := range context.HeaderExtensions() {
			if ext.URI == AudioLevelURI {
				t.audioLevelExtID = uint8(ext.ID)
				break
			}
		}
		if t.audioLevelExtID != 0 {
			t.audioLevelByPT = make(map[uint8]*audioLevelComputer)
			for _, p := range context.CodecParameters() {
				if _, exists := t.audioLevelByPT[uint8(p.PayloadType)]; !exists {
					t.audioLevelByPT[uint8(p.PayloadType)] = newAudioLevelComputer(p.MimeType)
				}
			}
		}
	}
	t.mu.Unlock()

	if chosen.MimeType != "" {
		return chosen, nil
	}
	return webrtc.RTPCodecParameters{}, nil
}

func (t *Track) Unbind(context webrtc.TrackLocalContext) error {
	t.mu.Lock()
	t.writer = nil
	t.audioLevelExtID = 0
	t.audioLevelByPT = nil
	t.mu.Unlock()
	return nil
}

func (t *Track) ID() string {
	return t.id
}

func (t *Track) RID() string {
	return "" // don't know what it is
}

func (t *Track) StreamID() string {
	return t.streamID
}

func (t *Track) Kind() webrtc.RTPCodecType {
	return webrtc.NewRTPCodecType(t.kind)
}

func (t *Track) WriteRTP(payloadType uint8, packet *rtp.Packet) (err error) {
	// using mutex because Unbind https://github.com/AlexxIT/go2rtc/issues/994
	t.mu.Lock()

	// in case when we start WriteRTP before Track.Bind
	if t.writer != nil {
		// important to have internal counter if input packets from different sources
		t.sequence++

		header := packet.Header
		header.SSRC = t.ssrc
		header.PayloadType = payloadType
		header.SequenceNumber = t.sequence

		if t.audioLevelExtID != 0 {
			if computer, ok := t.audioLevelByPT[payloadType]; ok && computer != nil {
				level, voice := computer.level(packet.Payload)
				t.setAudioLevelExt(&header, level, voice)
			}
		}

		_, err = t.writer.WriteRTP(&header, packet.Payload)
	}

	t.mu.Unlock()
	return
}

// setAudioLevelExt writes the 1-byte audio-level extension (RFC 6464) into
// the RTP header. Format:
//
//	+-+-+-+-+-+-+-+-+
//	|V|   level     |
//	+-+-+-+-+-+-+-+-+
//
// V = voice flag (top bit), level = 0..127 dBov in the low 7 bits.
// We write directly via SetExtension — pion will allocate the extension
// profile/length on the wire.
func (t *Track) setAudioLevelExt(header *rtp.Header, level uint8, voice bool) {
	if level > 127 {
		level = 127
	}
	b := level & 0x7F
	if voice {
		b |= 0x80
	}
	_ = header.SetExtension(t.audioLevelExtID, []byte{b})
}
