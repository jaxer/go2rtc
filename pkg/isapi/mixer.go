package isapi

import (
	"errors"
	"math"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

const frameSamples = 160              // 20 ms, mono 8 kHz G.711
const queueSamples = frameSamples * 4 // at most 80 ms of speech per participant
const maxParticipants = 8

// speechMixer is driven by one camera clock, never by individual packet arrivals.
// It is deliberately G.711-only: no process, resampler or codec negotiation is
// hidden here. WebRTC/RTSP clients negotiate the camera's advertised G.711 codec.
type speechMixer struct {
	mu          sync.Mutex
	inputs      map[*core.Receiver]*mixInput
	output      string
	maxGain     float64
	targetRMS   float64
	compression *[256]byte // optional output-law lookup; no per-frame allocation/transcendentals
	limiter     float64
	dropped     uint64
}

type mixInput struct {
	source          *core.Receiver
	sender          *core.Sender
	codec           string
	samples         [queueSamples]int16
	head, size      int
	seq             uint16
	timestamp, ssrc uint32
	received        time.Time
	seen            bool
	hpX, hpY, gain  float64
	gainFrames      uint8
	frame           [frameSamples]float64
}

func newSpeechMixer(codec string, gainDB float64) *speechMixer {
	return &speechMixer{inputs: make(map[*core.Receiver]*mixInput), output: codec, maxGain: math.Pow(10, gainDB/20), targetRMS: 4096, limiter: 1}
}

func (m *speechMixer) add(media *core.Media, source *core.Receiver) (*core.Sender, error) {
	if source == nil || source.Codec == nil || source.Closed() {
		return nil, errors.New("isapi: microphone closed")
	}
	codec := source.Codec
	if (codec.Name != core.CodecPCMU && codec.Name != core.CodecPCMA) || codec.ClockRate != 8000 || codec.Channels > 1 {
		return nil, errors.New("isapi: mixer requires mono G.711 at 8000 Hz")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if input := m.inputs[source]; input != nil {
		return input.sender, nil
	}
	// Reclaim ended participants before enforcing the bounded input count.
	for key := range m.inputs {
		if key.Closed() {
			delete(m.inputs, key)
		}
	}
	if len(m.inputs) >= maxParticipants {
		return nil, errors.New("isapi: too many microphones")
	}
	input := &mixInput{source: source, codec: codec.Name, gain: 1}
	sender := core.NewSender(media, codec)
	input.sender = sender
	sender.Handler = func(packet *rtp.Packet) { m.push(input, packet, time.Now()) }
	m.inputs[source] = input
	sender.HandleRTP(source)
	return sender, nil
}

func (m *speechMixer) push(input *mixInput, packet *rtp.Packet, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inputs[input.source] != input || len(packet.Payload) == 0 || len(packet.Payload) > queueSamples {
		return
	}
	if input.seen && packet.SSRC == input.ssrc && now.Sub(input.received) < 500*time.Millisecond {
		if int16(packet.SequenceNumber-input.seq) <= 0 {
			return
		} // duplicate/late; uint16 wrap is intentional
		gap := int32(packet.Timestamp - input.timestamp)
		if gap < 0 {
			return
		}
		if gap > queueSamples {
			input.head = 0
			input.size = 0
		} else {
			for ; gap > 0; gap-- {
				m.append(input, 0)
			}
		}
	} else {
		input.head = 0
		input.size = 0
		input.hpX = 0
		input.hpY = 0
		input.gain = 1
		input.gainFrames = 0
	}
	input.seen = true
	input.received = now
	input.seq = packet.SequenceNumber
	input.ssrc = packet.SSRC
	input.timestamp = packet.Timestamp + uint32(len(packet.Payload))
	for _, value := range packet.Payload {
		sample := pcm.PCMUtoPCM(value)
		if input.codec == core.CodecPCMA {
			sample = pcm.PCMAtoPCM(value)
		}
		m.append(input, sample)
	}
}
func (m *speechMixer) append(input *mixInput, sample int16) {
	if input.size == queueSamples {
		input.head = (input.head + 1) % queueSamples
		input.size--
		m.dropped++
	}
	input.samples[(input.head+input.size)%queueSamples] = sample
	input.size++
}

// read consumes at most one frame from each microphone. Missing input contributes
// actual PCM zero; a silent participant never attenuates another participant.
// Caller owns out and reuses it. No allocations on the processing path.
func (m *speechMixer) read(out []byte, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var mix [frameSamples]float64
	any := false
	for key, input := range m.inputs {
		if key.Closed() {
			delete(m.inputs, key)
			continue
		}
		if now.Sub(input.received) > 100*time.Millisecond {
			input.size = 0
			input.hpX = 0
			input.hpY = 0
			continue
		}
		n := min(input.size, frameSamples)
		if n == 0 {
			continue
		}
		any = true
		var energy float64
		for i := 0; i < frameSamples; i++ {
			x := 0.0
			if i < n {
				x = float64(input.samples[input.head])
				input.head = (input.head + 1) % queueSamples
				input.size--
			}
			// 75 Hz DC/rumble rejection. No hard speech gate that would discard whispers.
			y := x - input.hpX + 0.9428*input.hpY
			input.hpX = x
			input.hpY = y
			input.frame[i] = y
			energy += y * y
		}
		target := 1.0
		rms := math.Sqrt(energy / frameSamples)
		if m.maxGain > 1 && rms >= 32 { // don't drive gain upward on digital silence
			target = math.Min(m.maxGain, m.targetRMS/rms)
		} else if rms < 32 {
			target = input.gain
		}
		// Initialize from the first non-silent block instead of making every new
		// hold wait up to four seconds for quiet speech amplification. Ramp within
		// this frame, with the final peak limiter still protecting the output.
		if rms >= 32 && m.maxGain > 1 {
			if input.gainFrames != 0 {
				rise := 1.013911 // 6 dB/s once speech has settled
				if input.gainFrames < 50 {
					rise = 1.148154 // 60 dB/s during the first second of active audio
				}
				target = math.Max(input.gain*0.870964, math.Min(input.gain*rise, target))
			}
			if input.gainFrames < 50 {
				input.gainFrames++
			}
		}
		start := input.gain
		for i, x := range input.frame {
			mix[i] += x * (start + (target-start)*float64(i+1)/frameSamples)
		}
		input.gain = target
	}
	if !any {
		return false
	}
	peak := 0.0
	for _, x := range mix {
		peak = math.Max(peak, math.Abs(x))
	}
	const ceiling = 28000.0 // headroom after G.711 quantization; applies even with AGC off
	wanted := 1.0
	if peak > ceiling {
		wanted = ceiling / peak
	}
	// Immediate frame attack; smooth 120 ms release. Frame buffering adds no extra lookahead.
	m.limiter = math.Min(wanted, m.limiter+(1-m.limiter)*0.153518)
	for i, x := range mix {
		sample := int16(math.Round(math.Max(-ceiling, math.Min(ceiling, x*m.limiter))))
		if m.output == core.CodecPCMA {
			out[i] = pcm.PCMtoPCMA(sample)
		} else {
			out[i] = pcm.PCMtoPCMU(sample)
		}
		if m.compression != nil {
			out[i] = m.compression[out[i]]
		}
	}
	return true
}

// moderateCompression reproduces the accepted real-recording comparison:
// existing limited G.711 output -> decode -> 28000*tanh(2*x/28000) -> encode.
// Retaining the intermediate quantization makes the live result match the sample.
// This soft saturation trades waveform fidelity for higher average speech level.
func moderateCompression(codec string) *[256]byte {
	table := new([256]byte)
	for i := range table {
		x := pcm.PCMUtoPCM(byte(i))
		if codec == core.CodecPCMA {
			x = pcm.PCMAtoPCM(byte(i))
		}
		y := int16(math.Round(28000 * math.Tanh(2*float64(x)/28000)))
		if codec == core.CodecPCMA {
			table[i] = pcm.PCMtoPCMA(y)
		} else {
			table[i] = pcm.PCMtoPCMU(y)
		}
	}
	return table
}
