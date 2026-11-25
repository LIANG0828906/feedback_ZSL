package vpcompose

import "math"

var srgbToLinear16 = buildSRGBLUT()

func buildSRGBLUT() [256]uint16 {
	var lut [256]uint16
	for i := range lut {
		value := float64(i) / 255.0
		var linear float64
		if value <= 0.04045 {
			linear = value / 12.92
		} else {
			linear = math.Pow((value+0.055)/1.055, 2.4)
		}
		lut[i] = uint16(math.Round(linear * 65535.0))
	}
	return lut
}

func SRGB8ToLinear16(channel uint8) uint16 {
	return srgbToLinear16[channel]
}

func mul16(a, b uint32) uint32 {
	if a == 0 || b == 0 {
		return 0
	}
	return (a*b + 32767) / 65535
}

func clamp16(value uint32) uint16 {
	if value > 65535 {
		return 65535
	}
	return uint16(value)
}

func blendChannel(mode BlendMode, backdrop, source uint16) uint16 {
	cb := uint32(backdrop)
	cs := uint32(source)
	var value uint32
	switch mode {
	case BlendMultiply:
		value = mul16(cb, cs)
	case BlendScreen:
		value = cb + cs - mul16(cb, cs)
	case BlendAdd:
		value = cb + cs
	default:
		value = cs
	}
	return clamp16(value)
}

// compositePixel applies the W3C compositing equation in a 16-bit linear,
// premultiplied working space. srcPremul contains premultiplied RGB and alpha.
// All divisions are explicit integer rounding.
func compositePixel(dst [4]uint16, srcPremul [4]uint16, mode BlendMode) [4]uint16 {
	as := uint32(srcPremul[3])
	if as == 0 {
		return dst
	}
	da := uint32(dst[3])
	if as == 65535 && da == 0 {
		return srcPremul
	}
	cb := [4]uint16{}
	if da > 0 {
		cb[0] = UnpremultiplyChannel(dst[0], uint16(da))
		cb[1] = UnpremultiplyChannel(dst[1], uint16(da))
		cb[2] = UnpremultiplyChannel(dst[2], uint16(da))
	}
	out := [4]uint16{}
	oneMinusAs := uint32(65535) - as
	for i := 0; i < 3; i++ {
		termDst := mul16(oneMinusAs, uint32(dst[i]))
		termSrc := mul16(uint32(65535)-da, uint32(srcPremul[i]))
		var termBlend uint32
		if da > 0 {
			cs := UnpremultiplyChannel(srcPremul[i], uint16(as))
			bs := blendChannel(mode, cb[i], cs)
			termBlend = mul16(as, mul16(da, uint32(bs)))
		}
		out[i] = clamp16(termDst + termSrc + termBlend)
	}
	out[3] = clamp16(as + mul16(da, oneMinusAs))
	return out
}
