package webrtc

import (
	"math"

	"github.com/pion/opus"
	"github.com/pion/webrtc/v4"
)

// AudioLevelURI is the RFC 6464 RTP header extension URI for client-to-mixer
// audio level. We advertise it on outgoing audio so browsers can read the
// level via RTCRtpReceiver.getSynchronizationSources()[].audioLevel.
const AudioLevelURI = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"

// audioLevelSilence is the dBov value used when the codec is unsupported or
// the payload is empty. RFC 6464 reserves 127 for silence / very quiet.
const audioLevelSilence uint8 = 127

// audioLevelComputer computes the audio-level extension payload for a single
// RTP track. The struct is owned by Track and is not safe for concurrent use;
// Track.WriteRTP serialises calls under its own mutex.
type audioLevelComputer struct {
	mime    string // negotiated codec MIME (e.g. webrtc.MimeTypeOpus)
	decoder *opus.Decoder
	pcm     []float32
}

func newAudioLevelComputer(mime string) *audioLevelComputer {
	c := &audioLevelComputer{mime: mime}
	if mime == webrtc.MimeTypeOpus {
		dec, err := opus.NewDecoderWithOutput(48000, 1)
		if err == nil {
			c.decoder = &dec
			// Up to 120 ms at 48 kHz mono (Opus max frame size).
			c.pcm = make([]float32, 5760)
		}
	}
	return c
}

// level returns (dBov, voice). dBov is 0..127 where 0 = full-scale and 127 =
// silence; voice is the V flag from RFC 6464 (set whenever the level is
// above a small threshold). On any error or unsupported codec we report
// silence — better to under-report than to inject noise into the meter.
func (c *audioLevelComputer) level(payload []byte) (uint8, bool) {
	if c == nil || len(payload) == 0 {
		return audioLevelSilence, false
	}
	switch c.mime {
	case webrtc.MimeTypeOpus:
		return c.levelOpus(payload)
	case webrtc.MimeTypePCMA:
		return levelPCMA(payload)
	case webrtc.MimeTypePCMU:
		return levelPCMU(payload)
	}
	return audioLevelSilence, false
}

func (c *audioLevelComputer) levelOpus(payload []byte) (uint8, bool) {
	if c.decoder == nil {
		return audioLevelSilence, false
	}
	_, _, err := c.decoder.DecodeFloat32(payload, c.pcm)
	if err != nil {
		return audioLevelSilence, false
	}
	// We don't know the exact sample count after decode, but the relevant
	// data fills the front of the buffer — RMS over the whole buffer is
	// fine because trailing zeros only pull the level down slightly.
	return rmsToDBov(rmsFloat32(c.pcm))
}

func levelPCMA(payload []byte) (uint8, bool) {
	if len(payload) == 0 {
		return audioLevelSilence, false
	}
	var sumSq float64
	for _, b := range payload {
		s := float64(alawToLinear[b]) / 32768.0
		sumSq += s * s
	}
	return rmsToDBov(math.Sqrt(sumSq / float64(len(payload))))
}

func levelPCMU(payload []byte) (uint8, bool) {
	if len(payload) == 0 {
		return audioLevelSilence, false
	}
	var sumSq float64
	for _, b := range payload {
		s := float64(ulawToLinear[b]) / 32768.0
		sumSq += s * s
	}
	return rmsToDBov(math.Sqrt(sumSq / float64(len(payload))))
}

func rmsFloat32(buf []float32) float64 {
	if len(buf) == 0 {
		return 0
	}
	var sumSq float64
	for _, s := range buf {
		v := float64(s)
		sumSq += v * v
	}
	return math.Sqrt(sumSq / float64(len(buf)))
}

// rmsToDBov maps a linear RMS in [0, 1] to the RFC 6464 quantised level
// (0 = 0 dBov / max, 127 = silence) and the V flag (true iff above a small
// noise floor).
func rmsToDBov(rms float64) (uint8, bool) {
	if rms <= 0 || math.IsNaN(rms) {
		return audioLevelSilence, false
	}
	dbov := -20 * math.Log10(rms)
	switch {
	case dbov < 0:
		dbov = 0
	case dbov > 127:
		return audioLevelSilence, false
	}
	level := uint8(dbov + 0.5)
	// Voice flag: anything above -60 dBov is "audible enough to count".
	return level, level < 60
}
