package isapi

import (
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"math"
	"testing"
	"time"
)

func TestModerateCompressionMatchesAcceptedTransform(t *testing.T) {
	for _, codec := range []string{core.CodecPCMU, core.CodecPCMA} {
		t.Run(codec, func(t *testing.T) {
			decode := pcm.PCMUtoPCM
			encode := pcm.PCMtoPCMU
			if codec == core.CodecPCMA {
				decode = pcm.PCMAtoPCM
				encode = pcm.PCMtoPCMA
			}
			table := moderateCompression(codec)
			// Exhaustively check both G.711 alphabets, including their zero codes.
			for i := 0; i < 256; i++ {
				want := encode(int16(math.Round(28000 * math.Tanh(2*float64(decode(byte(i)))/28000))))
				if table[i] != want {
					t.Fatalf("code %d: got %d want %d", i, table[i], want)
				}
				if math.Abs(float64(decode(table[i]))) > 28500 {
					t.Fatal("peak ceiling exceeded")
				}
			}
			for _, participants := range []int{1, 3, 8} {
				plain := newSpeechMixer(codec, 24)
				plain.targetRMS = 32768 * math.Pow(10, -6.0/20)
				compressed := newSpeechMixer(codec, 24)
				compressed.targetRMS = plain.targetRMS
				compressed.compression = table
				a, b := make([]*mixInput, participants), make([]*mixInput, participants)
				for i := range a {
					a[i] = testInput(plain, codec)
					b[i] = testInput(compressed, codec)
				}
				now := time.Now()
				out, baseline := make([]byte, 160), make([]byte, 160)
				var energyBefore, energyAfter float64
				for frame := 0; frame < 100; frame++ {
					for i := range a {
						// Include silence, quiet starts, loud overlaps and changing levels.
						amplitude := float64((frame*173 + i*991) % 24000)
						if frame < 4 {
							amplitude = 0
						}
						data := tone(codec, amplitude, 3+i)
						feed(plain, a[i], data, uint16(frame), uint32(frame*160), now)
						feed(compressed, b[i], data, uint16(frame), uint32(frame*160), now)
					}
					plain.read(baseline, now)
					compressed.read(out, now)
					for i, v := range baseline {
						if out[i] != table[v] {
							t.Fatalf("%d participants, frame %d: differs from accepted processing", participants, frame)
						}
						x, y := float64(decode(v)), float64(decode(out[i]))
						energyBefore += x * x
						energyAfter += y * y
					}
					now = now.Add(20 * time.Millisecond)
				}
				if energyAfter <= energyBefore {
					t.Fatal("did not raise average output")
				}
			}
		})
	}
}

func BenchmarkMixerModerate(b *testing.B) {
	m := newSpeechMixer(core.CodecPCMU, 24)
	m.targetRMS = 32768 * math.Pow(10, -6.0/20)
	m.compression = moderateCompression(core.CodecPCMU)
	in := testInput(m, core.CodecPCMU)
	data := tone(core.CodecPCMU, 2000, 5)
	out := make([]byte, 160)
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		feed(m, in, data, uint16(i), uint32(i*160), now)
		m.read(out, now)
		now = now.Add(20 * time.Millisecond)
	}
}
