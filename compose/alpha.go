package compose

import (
	"math"

	"virtualstudio/model"
)

// These conversions use an encoded-domain alpha convention deliberately: the
// 8-bit RGB value is multiplied by alpha before linearization. This mirrors
// common interchange files and, importantly, allows deterministic byte-level
// conversion between straight and premultiplied source documents.
func NormalizePixels(src []byte, mode model.AlphaMode) []byte {
	out := append([]byte(nil), src...)
	if mode == model.AlphaStraight {
		for i := 0; i+3 < len(out); i += 4 {
			if out[i+3] == 0 {
				out[i], out[i+1], out[i+2] = 0, 0, 0
			}
		}
		return out
	}
	for i := 0; i+3 < len(out); i += 4 {
		a := int(out[i+3])
		if a == 0 {
			out[i], out[i+1], out[i+2] = 0, 0, 0
			continue
		}
		for c := 0; c < 3; c++ {
			v := (int(out[i+c])*255 + a/2) / a
			if v > 255 {
				v = 255
			}
			out[i+c] = byte(v)
		}
	}
	return out
}

// PremultiplyPixels16 emits 16-bit RGBA samples. The extra precision makes the
// 8-bit straight -> premultiplied -> 8-bit straight conversion lossless for
// every non-zero alpha value.
func PremultiplyPixels16(straight []byte) []byte {
	pixels := len(straight) / 4
	out := make([]byte, pixels*8)
	for p := 0; p < pixels; p++ {
		in := p * 4
		dst := p * 8
		a := int64(straight[in+3])
		a16 := a * 257
		out[dst+6] = byte(a16)
		out[dst+7] = byte(a16 >> 8)
		if a == 0 {
			continue
		}
		for c := 0; c < 3; c++ {
			v := (int64(straight[in+c])*a*257 + 127) / 255
			out[dst+c*2] = byte(v)
			out[dst+c*2+1] = byte(v >> 8)
		}
	}
	return out
}

func NormalizePixels16(src []byte, mode model.AlphaMode) []byte {
	pixels := len(src) / 8
	out := make([]byte, pixels*4)
	for p := 0; p < pixels; p++ {
		in := p * 8
		dst := p * 4
		a16 := int64(src[in+6]) | int64(src[in+7])<<8
		a8 := (a16 + 128) / 257
		if a8 > 255 {
			a8 = 255
		}
		out[dst+3] = byte(a8)
		if a8 == 0 {
			continue
		}
		for c := 0; c < 3; c++ {
			v16 := int64(src[in+c*2]) | int64(src[in+c*2+1])<<8
			var v int64
			if mode == model.AlphaStraight {
				v = (v16*255 + 32767) / 65535
			} else {
				v = (v16*255 + a16/2) / a16
			}
			if v > 255 {
				v = 255
			}
			out[dst+c] = byte(v)
		}
	}
	return out
}

func PremultiplyPixels(straight []byte) []byte {
	out := append([]byte(nil), straight...)
	for i := 0; i+3 < len(out); i += 4 {
		a := int(out[i+3])
		for c := 0; c < 3; c++ {
			v := (int(out[i+c])*a + 127) / 255
			out[i+c] = byte(v)
		}
	}
	return out
}

func DecodeSRGB(v uint8) float64 {
	x := float64(v) / 255.0
	if x <= 0.04045 {
		return x / 12.92
	}
	return math.Pow((x+0.055)/1.055, 2.4)
}

func EncodeSRGB(linear float64) uint8 {
	if linear <= 0 {
		return 0
	}
	if linear >= 1 {
		return 255
	}
	var x float64
	if linear <= 0.0031308 {
		x = linear * 12.92
	} else {
		x = 1.055*math.Pow(linear, 1.0/2.4) - 0.055
	}
	return uint8(math.Round(clamp01(x) * 255.0))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
