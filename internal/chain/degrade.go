package chain

import (
	"math"

	"qkernel/internal/imagex"
)

func ApplyStep(input *imagex.Frame, step Step, index int, seed int64) *imagex.Frame {
	out := input.Clone()
	tile := RenderTile(input, step, index, seed, input.Bounds())
	out.CopyRectFrom(input.Bounds(), tile)
	return out
}

func RenderTile(input *imagex.Frame, step Step, index int, seed int64, tile imagex.Rect) []byte {
	tile = tile.Intersect(input.Bounds())
	tileW, tileH := tile.Dx(), tile.Dy()
	ch := input.Channels
	dst := make([]byte, tileW*tileH*ch)
	effectRegion, _ := step.Region.Resolve(input.Bounds())

	for y := tile.MinY; y < tile.MaxY; y++ {
		row := (y - tile.MinY) * tileW * ch
		for x := tile.MinX; x < tile.MaxX; x++ {
			li := row + (x-tile.MinX)*ch
			src := input.Pixel(x, y)
			if !effectRegion.Contains(x, y) {
				copy(dst[li:li+ch], src)
				continue
			}
			r, g, b := degradePixel(input, step, index, seed, x, y)
			dst[li] = clampByte(r)
			dst[li+1] = clampByte(g)
			dst[li+2] = clampByte(b)
			if ch == 4 {
				dst[li+3] = src[3]
			}
		}
	}
	return dst
}

func degradePixel(f *imagex.Frame, step Step, index int, seed int64, x, y int) (float64, float64, float64) {
	or, og, ob := f.RGBAtClamped(x, y)
	r, g, b := float64(or), float64(og), float64(ob)
	switch step.Type {
	case NoiseStep:
		amp := step.Strength * 42
		n1 := deterministicUnit(seed, x, y, 1)
		n2 := deterministicUnit(seed, x, y, 2)
		n3 := deterministicUnit(seed, x, y, 3)
		r += (n1*2 - 1) * amp
		g += (n2*2 - 1) * amp
		b += (n3*2 - 1) * amp
	case BlurStep:
		radius := int(math.Round(step.Strength * 4))
		if radius <= 0 {
			radius = 1
		}
		var sr, sg, sb, count float64
		for dy := -radius; dy <= radius; dy++ {
			for dx := -radius; dx <= radius; dx++ {
				rr, gg, bb := f.RGBAtClamped(x+dx, y+dy)
				weight := 1.0
				if dx != 0 || dy != 0 {
					dist := math.Hypot(float64(dx), float64(dy))
					weight = 1.0 / (1.0 + dist)
				}
				sr += float64(rr) * weight
				sg += float64(gg) * weight
				sb += float64(bb) * weight
				count += weight
			}
		}
		r, g, b = sr/count, sg/count, sb/count
	case BlockStep:
		ar, ag, ab := blockAverage(f, x, y)
		mix := 0.25 + 0.55*step.Strength
		r = r*(1-mix) + ar*mix
		g = g*(1-mix) + ag*mix
		b = b*(1-mix) + ab*mix
		levels := int(2 + (1-step.Strength)*30)
		if levels < 2 {
			levels = 2
		}
		r = quantize(r, levels)
		g = quantize(g, levels)
		b = quantize(b, levels)
	case ChromaStep:
		shift := int(math.Round(step.Strength * 10))
		if shift <= 0 {
			shift = 1
		}
		sr8, sg8, sb8 := f.RGBAtClamped(x+shift, y)
		yy, _, _ := rgbToYCbCr(r, g, b)
		_, cb, cr := rgbToYCbCr(float64(sr8), float64(sg8), float64(sb8))
		cb += 24 * step.Strength
		cr -= 18 * step.Strength
		r, g, b = yCbCrToRGB(yy, cb, cr)
	case OverexposeStep:
		k := 0.88 * step.Strength
		r = r + (255-r)*k
		g = g + (255-g)*k
		b = b + (255-b)*k
	}
	return r, g, b
}

func blockAverage(f *imagex.Frame, x, y int) (float64, float64, float64) {
	x0 := x &^ 7
	y0 := y &^ 7
	x1 := minInt(x0+8, f.Width)
	y1 := minInt(y0+8, f.Height)
	x0 = maxInt(x0, 0)
	y0 = maxInt(y0, 0)
	var r, g, b, n float64
	for yy := y0; yy < y1; yy++ {
		for xx := x0; xx < x1; xx++ {
			pr, pg, pb := f.RGBAtClamped(xx, yy)
			r += float64(pr)
			g += float64(pg)
			b += float64(pb)
			n++
		}
	}
	return r / n, g / n, b / n
}

func quantize(v float64, levels int) float64 {
	q := math.Round(v / 255 * float64(levels-1))
	return q / float64(levels-1) * 255
}

func rgbToYCbCr(r, g, b float64) (float64, float64, float64) {
	y := 0.299*r + 0.587*g + 0.114*b
	cb := 128 - 0.168736*r - 0.331264*g + 0.5*b
	cr := 128 + 0.5*r - 0.418688*g - 0.081312*b
	return y, cb, cr
}

func yCbCrToRGB(y, cb, cr float64) (float64, float64, float64) {
	r := y + 1.402*(cr-128)
	g := y - 0.344136*(cb-128) - 0.714136*(cr-128)
	b := y + 1.772*(cb-128)
	return r, g, b
}

func clampByte(v float64) byte {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(math.Round(v))
}

func deterministicUnit(seed int64, values ...int) float64 {
	h := splitMix64(uint64(seed) ^ 0x9E3779B97F4A7C15)
	for i, v := range values {
		h = splitMix64(h ^ (uint64(v) + uint64(i)*0xD1B54A32D192ED03 + 0x9E3779B97F4A7C15))
	}
	return float64(h>>11) * (1.0 / float64(uint64(1)<<53))
}

func splitMix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
