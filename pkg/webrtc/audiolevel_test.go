package webrtc

import (
	"math"
	"testing"
)

func TestRMSToDBov(t *testing.T) {
	cases := []struct {
		name      string
		rms       float64
		wantLevel uint8 // approximate; we allow ±1
		wantVoice bool
	}{
		{"silence", 0, audioLevelSilence, false},
		{"full scale", 1.0, 0, true},
		{"-20 dBov", 0.1, 20, true},
		{"-40 dBov", 0.01, 40, true},
		{"-60 dBov", 0.001, 60, false},
		{"way below floor", 1e-10, audioLevelSilence, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotLevel, gotVoice := rmsToDBov(c.rms)
			if c.rms <= 0 || c.rms < 1e-7 {
				if gotLevel != audioLevelSilence {
					t.Fatalf("expected silence (%d), got %d", audioLevelSilence, gotLevel)
				}
				return
			}
			if math.Abs(float64(gotLevel)-float64(c.wantLevel)) > 1 {
				t.Fatalf("level: got %d want ~%d", gotLevel, c.wantLevel)
			}
			if gotVoice != c.wantVoice {
				t.Fatalf("voice: got %v want %v", gotVoice, c.wantVoice)
			}
		})
	}
}

func TestPCMASilence(t *testing.T) {
	// G.711 A-law silence is 0xD5 (the codeword for 0).
	silent := make([]byte, 160)
	for i := range silent {
		silent[i] = 0xD5
	}
	level, voice := levelPCMA(silent)
	// A-law's silence codeword (0xD5) has an inherent ±8/32768 quantization
	// floor (~ -72 dBov), so we accept anything ≥ 60.
	if level < 60 || voice {
		t.Fatalf("PCMA silence: level=%d voice=%v (expected level≥60, voice=false)", level, voice)
	}
}

func TestPCMUSilence(t *testing.T) {
	// G.711 μ-law silence is 0xFF (the codeword for 0).
	silent := make([]byte, 160)
	for i := range silent {
		silent[i] = 0xFF
	}
	level, voice := levelPCMU(silent)
	if level < 90 || voice {
		t.Fatalf("PCMU silence: level=%d voice=%v (expected level≥90, voice=false)", level, voice)
	}
}
