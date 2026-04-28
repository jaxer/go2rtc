package webrtc

// G.711 A-law / μ-law decode tables, generated at init() so the file stays
// short. Each table maps the 8-bit codeword to a signed 16-bit linear
// sample.

var (
	alawToLinear [256]int16
	ulawToLinear [256]int16
)

func init() {
	for i := 0; i < 256; i++ {
		alawToLinear[i] = decodeALaw(byte(i))
		ulawToLinear[i] = decodeULaw(byte(i))
	}
}

// decodeALaw decodes one G.711 A-law codeword. Reference: ITU-T G.711.
func decodeALaw(a byte) int16 {
	a ^= 0x55
	sign := a & 0x80
	exponent := (a & 0x70) >> 4
	mantissa := a & 0x0F
	var sample int32
	if exponent != 0 {
		sample = (int32(mantissa)<<4 + 0x108) << (exponent - 1)
	} else {
		sample = int32(mantissa)<<4 + 0x008
	}
	if sign == 0 {
		sample = -sample
	}
	return int16(sample)
}

// decodeULaw decodes one G.711 μ-law codeword. Reference: ITU-T G.711.
func decodeULaw(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exponent := (u & 0x70) >> 4
	mantissa := u & 0x0F
	sample := (int32(mantissa)<<3 + 0x84) << exponent
	sample -= 0x84
	if sign != 0 {
		sample = -sample
	}
	return int16(sample)
}
