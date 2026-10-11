package isapi

import (
	"bytes"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

func testInput(m *speechMixer, codec string) *mixInput {
	r := core.NewReceiver(nil, &core.Codec{Name: codec, ClockRate: 8000})
	in := &mixInput{source: r, codec: codec, gain: 1}
	m.mu.Lock()
	m.inputs[r] = in
	m.mu.Unlock()
	return in
}
func tone(codec string, amplitude float64, cycle int) []byte {
	out := make([]byte, frameSamples)
	for i := range out {
		sample := int16(amplitude * math.Sin(2*math.Pi*float64(i*cycle)/frameSamples))
		if codec == core.CodecPCMA {
			out[i] = pcm.PCMtoPCMA(sample)
		} else {
			out[i] = pcm.PCMtoPCMU(sample)
		}
	}
	return out
}
func feed(m *speechMixer, in *mixInput, data []byte, seq uint16, ts uint32, now time.Time) {
	m.push(in, &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: ts, SSRC: 1}, Payload: data}, now)
}
func level(data []byte) float64 {
	var sum float64
	for _, v := range data {
		x := float64(pcm.PCMUtoPCM(v))
		sum += x * x
	}
	return math.Sqrt(sum / float64(len(data)))
}
func TestMixerSumsWithoutSilentAttenuation(t *testing.T) {
	now := time.Now()
	data := tone(core.CodecPCMU, 2000, 10)
	render := func(count int, silent bool) []byte {
		m := newSpeechMixer(core.CodecPCMU, 0)
		for i := 0; i < count; i++ {
			feed(m, testInput(m, core.CodecPCMU), data, 1, 0, now)
		}
		if silent {
			feed(m, testInput(m, core.CodecPCMU), bytes.Repeat([]byte{255}, 160), 1, 0, now)
		}
		out := make([]byte, 160)
		if !m.read(out, now) {
			t.Fatal("missing audio")
		}
		return out
	}
	one := render(1, false)
	if !bytes.Equal(one, render(1, true)) {
		t.Fatal("silent participant changes another microphone")
	}
	ratio := level(render(2, false)) / level(one)
	if ratio < 1.9 || ratio > 2.1 {
		t.Fatalf("mixed gain = %f", ratio)
	}
}
func TestMixerGainLimiterAndCodecs(t *testing.T) {
	for _, codec := range []string{core.CodecPCMU, core.CodecPCMA} {
		t.Run(codec, func(t *testing.T) {
			m := newSpeechMixer(codec, 24)
			in := testInput(m, codec)
			now := time.Now()
			out := make([]byte, 160)
			for i := 0; i < 300; i++ {
				feed(m, in, tone(codec, 160, 10), uint16(i), uint32(i*160), now)
				m.read(out, now)
				now = now.Add(20 * time.Millisecond)
			}
			if in.gain < 15 || in.gain > 16 {
				t.Fatalf("quiet speech gain %f", in.gain)
			}
			for j := 0; j < 7; j++ {
				feed(m, testInput(m, codec), tone(codec, 30000, 10), 1, 0, now)
			}
			feed(m, in, tone(codec, 30000, 10), 300, 48000, now)
			m.read(out, now)
			for _, v := range out {
				sample := pcm.PCMUtoPCM(v)
				if codec == core.CodecPCMA {
					sample = pcm.PCMAtoPCM(v)
				}
				if math.Abs(float64(sample)) > 28500 {
					t.Fatalf("clipping: %d", sample)
				}
			}
		})
	}
}
func TestMixerPacketTimingAndBounds(t *testing.T) {
	m := newSpeechMixer(core.CodecPCMU, 0)
	in := testInput(m, core.CodecPCMU)
	now := time.Now()
	data := tone(core.CodecPCMU, 1000, 10)
	feed(m, in, data, 65535, ^uint32(0)-159, now)
	feed(m, in, data, 0, 0, now) // sequence and timestamp wrap
	if in.size != 320 {
		t.Fatal(in.size)
	}
	feed(m, in, data, 0, 0, now)
	feed(m, in, data, 65534, ^uint32(0)-319, now)
	if in.size != 320 {
		t.Fatal("duplicate/reordered packet was queued")
	}
	for i := 1; i < 10; i++ {
		feed(m, in, data, uint16(i), uint32(i*160), now)
	}
	if in.size != queueSamples || m.dropped == 0 {
		t.Fatal("queue not bounded")
	}
	out := make([]byte, 160)
	if m.read(out, now.Add(101*time.Millisecond)) || in.size != 0 {
		t.Fatal("stale speech replayed")
	}
	feed(m, in, data, 10, 1600, now.Add(time.Second))
	in.source.Close()
	if m.read(out, now.Add(time.Second)) || len(m.inputs) != 0 {
		t.Fatal("disconnected microphone retained")
	}
}
func TestMixerParticipantLifecycle(t *testing.T) {
	m := newSpeechMixer(core.CodecPCMU, 0)
	media := &core.Media{Kind: core.KindAudio}
	var sources []*core.Receiver
	var senders []*core.Sender
	defer func() {
		for _, s := range senders {
			s.Close()
			s.Wait()
		}
	}()
	for i := 0; i < maxParticipants; i++ {
		r := core.NewReceiver(media, &core.Codec{Name: core.CodecPCMU, ClockRate: 8000})
		sources = append(sources, r)
		s, err := m.add(media, r)
		if err != nil {
			t.Fatal(err)
		}
		senders = append(senders, s)
	}
	same, err := m.add(media, sources[0])
	if err != nil || same != senders[0] {
		t.Fatal("duplicate attachment")
	}
	extra := core.NewReceiver(media, sources[0].Codec)
	if _, err = m.add(media, extra); err == nil {
		t.Fatal("participant limit ignored")
	}
	sources[0].Close()
	s, err := m.add(media, extra)
	if err != nil {
		t.Fatal(err)
	}
	senders = append(senders, s)
	if _, err = m.add(media, sources[0]); err == nil {
		t.Fatal("closed source accepted")
	}
}
func TestMixerConcurrentPushRead(t *testing.T) {
	m := newSpeechMixer(core.CodecPCMU, 24)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		in := testInput(m, core.CodecPCMU)
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := tone(core.CodecPCMU, 500, 10)
			for j := 0; j < 500; j++ {
				feed(m, in, data, uint16(j), uint32(j*160), time.Now())
			}
		}()
	}
	out := make([]byte, 160)
	for i := 0; i < 500; i++ {
		m.read(out, time.Now())
	}
	wg.Wait()
}
func BenchmarkMixer(b *testing.B) {
	for _, count := range []int{1, 3, 8} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			m := newSpeechMixer(core.CodecPCMU, 24)
			inputs := make([]*mixInput, count)
			for i := range inputs {
				inputs[i] = testInput(m, core.CodecPCMU)
			}
			p := rtp.Packet{Header: rtp.Header{SSRC: 1}, Payload: tone(core.CodecPCMU, 500, 10)}
			var out [160]byte
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.SequenceNumber++
				p.Timestamp += 160
				now = now.Add(20 * time.Millisecond)
				for _, in := range inputs {
					m.push(in, &p, now)
				}
				m.read(out[:], now)
			}
		})
	}
}

func TestMixerHigherSpeechTargetKeepsPeakAndGainBounds(t *testing.T) {
	for _, codec := range []string{core.CodecPCMU, core.CodecPCMA} {
		t.Run(codec, func(t *testing.T) {
			var levels []float64
			for _, targetRMS := range []float64{4096, 8192} {
				m := newSpeechMixer(codec, 24)
				m.targetRMS = targetRMS
				in := testInput(m, codec)
				now := time.Now()
				out := make([]byte, 160)
				for i := 0; i < 300; i++ {
					feed(m, in, tone(codec, 2000, 10), uint16(i), uint32(i*160), now)
					m.read(out, now)
					now = now.Add(20 * time.Millisecond)
				}
				var energy float64
				for _, v := range out {
					x := pcm.PCMUtoPCM(v)
					if codec == core.CodecPCMA {
						x = pcm.PCMAtoPCM(v)
					}
					energy += float64(x) * float64(x)
				}
				levels = append(levels, math.Sqrt(energy/160))
				if in.gain > m.maxGain {
					t.Fatal("gain cap exceeded")
				}
				for j := 0; j < 7; j++ {
					feed(m, testInput(m, codec), tone(codec, 30000, 10), 1, 0, now)
				}
				feed(m, in, tone(codec, 30000, 10), 300, 48000, now)
				m.read(out, now)
				for _, v := range out {
					x := pcm.PCMUtoPCM(v)
					if codec == core.CodecPCMA {
						x = pcm.PCMAtoPCM(v)
					}
					if math.Abs(float64(x)) > 28500 {
						t.Fatal("higher speech target bypassed peak limiter")
					}
				}
			}
			if ratio := levels[1] / levels[0]; ratio < 1.9 || ratio > 2.1 {
				t.Fatalf("6dB target change: RMS ratio=%f", ratio)
			}
		})
	}
}

func TestMixerQuietSpeechDoesNotRampForSeconds(t *testing.T) {
	m := newSpeechMixer(core.CodecPCMU, 24)
	m.targetRMS = 8192
	in := testInput(m, core.CodecPCMU)
	out := make([]byte, 160)
	now := time.Now()
	// Initial digital silence must not consume startup adaptation or boost noise.
	for i := 0; i < 10; i++ {
		feed(m, in, bytes.Repeat([]byte{255}, 160), uint16(i), uint32(i*160), now)
		m.read(out, now)
		now = now.Add(20 * time.Millisecond)
	}
	if in.gainFrames != 0 || in.gain != 1 {
		t.Fatal("silence initialized speech gain")
	}
	for i := 10; i < 12; i++ {
		feed(m, in, tone(core.CodecPCMU, 160, 10), uint16(i), uint32(i*160), now)
		m.read(out, now)
		now = now.Add(20 * time.Millisecond)
	}
	if in.gain < 15 || in.gain > m.maxGain || level(out) < 1500 {
		t.Fatalf("first quiet syllable still soft: gain=%f RMS=%f", in.gain, level(out))
	}
	// A new source/hold must initialize again, not inherit stale speech levels.
	feed(m, in, tone(core.CodecPCMU, 160, 10), 0, 0, now.Add(time.Second))
	m.read(out, now.Add(time.Second))
	if in.gainFrames != 1 || in.gain < 15 {
		t.Fatal("new hold lost fast startup")
	}
}

func TestMixerRecoversFromLoudInitialTransient(t *testing.T) {
	m := newSpeechMixer(core.CodecPCMU, 24)
	m.targetRMS = 8192
	in := testInput(m, core.CodecPCMU)
	out := make([]byte, 160)
	now := time.Now()
	feed(m, in, tone(core.CodecPCMU, 30000, 10), 0, 0, now)
	m.read(out, now)
	for i := 1; i <= 35; i++ {
		now = now.Add(20 * time.Millisecond)
		feed(m, in, tone(core.CodecPCMU, 160, 10), uint16(i), uint32(i*160), now)
		m.read(out, now)
	}
	if in.gain < 15 || in.gain > m.maxGain {
		t.Fatalf("startup transient suppressed following quiet words: gain=%f", in.gain)
	}
}
