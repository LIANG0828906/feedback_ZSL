package yuv

import (
	"context"
	"math"
)

const fixedOne int64 = 1_000_000

func fixed(v float64) int64 {
	return int64(math.Round(v * float64(fixedOne)))
}

func clampCode(v int64, p *Plane) uint16 {
	if v < int64(p.QuantMin) {
		return p.QuantMin
	}
	if v > int64(p.QuantMax) {
		return p.QuantMax
	}
	return uint16(v)
}

func clampImage(v int64, bitDepth int) uint16 {
	max := int64(uint32(1)<<uint(bitDepth) - 1)
	if v < 0 {
		return 0
	}
	if v > max {
		return uint16(max)
	}
	return uint16(v)
}

func gaussian3Blur(p *Plane) []uint16 {
	w, h := p.Width, p.Height
	out := make([]uint16, len(p.Data))
	for y := 0; y < h; y++ {
		ym, yp := maxInt(0, y-1), minInt(h-1, y+1)
		for x := 0; x < w; x++ {
			xm, xp := maxInt(0, x-1), minInt(w-1, x+1)
			sum := int64(p.At(xm, ym)) + 2*int64(p.At(x, ym)) + int64(p.At(xp, ym)) +
				2*int64(p.At(xm, y)) + 4*int64(p.At(x, y)) + 2*int64(p.At(xp, y)) +
				int64(p.At(xm, yp)) + 2*int64(p.At(x, yp)) + int64(p.At(xp, yp))
			out[y*w+x] = uint16((sum + 8) / 16)
		}
	}
	return out
}

func Denoise(ctx context.Context, p *Plane, strength float64) (*Plane, error) {
	out := p.Clone()
	if strength <= 0 {
		return out, nil
	}
	blur := gaussian3Blur(p)
	s := fixed(strength)
	for y := 0; y < p.Height; y++ {
		if y%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for x := 0; x < p.Width; x++ {
			i := y*p.Width + x
			v := (int64(p.Data[i])*(fixedOne-s) + int64(blur[i])*s + fixedOne/2) / fixedOne
			out.Data[i] = clampCode(v, p)
		}
	}
	return out, nil
}

func Sharpen(ctx context.Context, p *Plane, amount, thresholdNorm float64) (*Plane, error) {
	out := p.Clone()
	if amount <= 0 {
		return out, nil
	}
	blur := gaussian3Blur(p)
	a := fixed(amount)
	rangeQ := int64(p.QuantMax-p.QuantMin) * fixedOne
	threshold := int64(math.Round(thresholdNorm * float64(rangeQ) / float64(fixedOne)))
	for y := 0; y < p.Height; y++ {
		if y%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for x := 0; x < p.Width; x++ {
			i := y*p.Width + x
			diff := int64(p.Data[i]) - int64(blur[i])
			if diff >= -threshold && diff <= threshold {
				continue
			}
			v := int64(p.Data[i]) + (a*diff+fixedOne/2)/fixedOne
			out.Data[i] = clampCode(v, p)
		}
	}
	return out, nil
}

func LocalContrast(ctx context.Context, p *Plane, strength float64, radius int) (*Plane, error) {
	out := p.Clone()
	if strength <= 0 {
		return out, nil
	}
	mean := boxMean(ctx, p, radius)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := fixed(strength)
	for y := 0; y < p.Height; y++ {
		if y%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for x := 0; x < p.Width; x++ {
			i := y*p.Width + x
			detail := int64(p.Data[i])*fixedOne - mean[i]
			v := int64(p.Data[i]) + (s*detail+fixedOne*fixedOne/2)/(fixedOne*fixedOne)
			out.Data[i] = clampCode(v, p)
		}
	}
	return out, nil
}

func boxMean(ctx context.Context, p *Plane, radius int) []int64 {
	w, h := p.Width, p.Height
	mean := make([]int64, len(p.Data))
	diameter := int64(radius*2 + 1)
	for y := 0; y < h; y++ {
		if y%64 == 0 && ctx.Err() != nil {
			return mean
		}
		for x := 0; x < w; x++ {
			var sum int64
			for dy := -radius; dy <= radius; dy++ {
				yy := clampInt(y+dy, 0, h-1)
				for dx := -radius; dx <= radius; dx++ {
					xx := clampInt(x+dx, 0, w-1)
					sum += int64(p.At(xx, yy))
				}
			}
			mean[y*w+x] = sum * fixedOne / diameter / diameter
		}
	}
	return mean
}

func BandLimitedSmooth(ctx context.Context, p *Plane, sigma float64) (*Plane, error) {
	radius := int(math.Ceil(2 * sigma))
	radius = clampInt(radius, 1, 8)
	weights := make([]int64, radius*2+1)
	var sum float64
	for i := -radius; i <= radius; i++ {
		v := math.Exp(-float64(i*i) / (2 * sigma * sigma))
		sum += v
		weights[i+radius] = int64(math.Round(v / sum * float64(fixedOne)))
	}
	var weightSum int64
	for _, v := range weights {
		weightSum += v
	}
	weights[radius] += fixedOne - weightSum

	horizontal := make([]uint16, len(p.Data))
	for y := 0; y < p.Height; y++ {
		if y%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for x := 0; x < p.Width; x++ {
			var acc int64
			for d := -radius; d <= radius; d++ {
				xx := clampInt(x+d, 0, p.Width-1)
				acc += int64(p.At(xx, y)) * weights[d+radius]
			}
			horizontal[y*p.Width+x] = uint16((acc + fixedOne/2) / fixedOne)
		}
	}
	out := p.Clone()
	for y := 0; y < p.Height; y++ {
		if y%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for x := 0; x < p.Width; x++ {
			var acc int64
			for d := -radius; d <= radius; d++ {
				yy := clampInt(y+d, 0, p.Height-1)
				acc += int64(horizontal[yy*p.Width+x]) * weights[d+radius]
			}
			out.Data[y*p.Width+x] = clampCode((acc+fixedOne/2)/fixedOne, p)
		}
	}
	return out, nil
}

func ChromaGain(ctx context.Context, p *Plane, gain, offsetNorm float64) (*Plane, error) {
	out := p.Clone()
	g := fixed(gain)
	off := int64(math.Round(offsetNorm * float64(p.QuantMax-p.QuantMin)))
	base := int64(p.QuantMin)
	for y := 0; y < p.Height; y++ {
		if y%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for x := 0; x < p.Width; x++ {
			i := y*p.Width + x
			v := base + ((int64(p.Data[i])-base)*g+fixedOne/2)/fixedOne + off
			out.Data[i] = clampCode(v, p)
		}
	}
	return out, nil
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

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
