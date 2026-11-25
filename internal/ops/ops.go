package ops

import (
	"fmt"
	"math"
	"math/bits"

	"qualitybench/internal/core"
	"qualitybench/internal/model"
)

const AlgorithmVersion = "qb-ops-v1"

type Context struct {
	Seed      int64
	StepIndex int
}

func ApplyStep(src *core.Frame, step model.Step, ctx Context) (*core.Frame, error) {
	if src == nil {
		return nil, fmt.Errorf("nil source frame")
	}
	if err := src.Validate(); err != nil {
		return nil, err
	}
	region := step.RegionRect(src.Width, src.Height)
	if !region.InFrame(src.Width, src.Height) {
		return nil, fmt.Errorf("step region %s outside frame %dx%d", region, src.Width, src.Height)
	}
	dst := src.Clone()
	if err := ApplyRegion(dst, src, step, region, ctx); err != nil {
		return nil, err
	}
	return dst, nil
}

// ApplyRegion writes only pixels inside region to dst. dst must initially contain
// the same content as src; pixels outside region are never modified.
func ApplyRegion(dst, src *core.Frame, step model.Step, region core.Rect, ctx Context) error {
	if dst == nil || src == nil {
		return fmt.Errorf("nil frame")
	}
	if dst.Width != src.Width || dst.Height != src.Height || dst.Channels != src.Channels {
		return fmt.Errorf("src/dst geometry mismatch")
	}
	if err := src.Validate(); err != nil {
		return err
	}
	if err := dst.Validate(); err != nil {
		return err
	}
	if step.Strength == 0 {
		return nil
	}
	switch step.Type {
	case model.StepNoise:
		applyNoise(dst, src, region, step.Strength, ctx)
	case model.StepBlur:
		applyBlur(dst, src, region, step.Strength)
	case model.StepBlockiness:
		applyBlockiness(dst, src, region, step.Strength)
	case model.StepChroma:
		applyChroma(dst, src, region, step.Strength)
	case model.StepOverexpose:
		applyOverexpose(dst, src, region, step.Strength)
	default:
		return fmt.Errorf("unknown step type %q", step.Type)
	}
	return nil
}

func ApplyBlock(src *core.Frame, step model.Step, block core.Rect, ctx Context) (*core.Frame, error) {
	if src == nil {
		return nil, fmt.Errorf("nil source frame")
	}
	if !block.InFrame(src.Width, src.Height) {
		return nil, fmt.Errorf("block %s outside frame %dx%d", block, src.Width, src.Height)
	}
	action := step.RegionRect(src.Width, src.Height)
	_, ok := core.Intersection(block, action)
	if !ok {
		return src.Block(block)
	}
	// Evaluate against the complete frame so blur boundaries, global 8x8 block
	// alignment and coordinate-addressed noise match the reference chain.
	full := src.Clone()
	if err := ApplyRegion(full, src, step, action, ctx); err != nil {
		return nil, err
	}
	out, err := full.Block(block)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func applyNoise(dst, src *core.Frame, region core.Rect, strength float64, ctx Context) {
	amplitude := strength * 28.0
	for y := region.Y; y < region.Bottom(); y++ {
		for x := region.X; x < region.Right(); x++ {
			o := src.Offset(x, y)
			for c := 0; c < 3 && c < src.Channels; c++ {
				n := deterministicGaussian(ctx.Seed, int64(ctx.StepIndex), x, y, c)
				v := float64(src.Pix[o+c]) + n*amplitude
				dst.Pix[o+c] = clampByte(v)
			}
		}
	}
}

func applyBlur(dst, src *core.Frame, region core.Rect, strength float64) {
	radius := 1 + int(math.Floor(strength*2))
	if radius > 3 {
		radius = 3
	}
	alpha := math.Min(1, strength)
	inv := 1 - alpha
	for y := region.Y; y < region.Bottom(); y++ {
		for x := region.X; x < region.Right(); x++ {
			o := src.Offset(x, y)
			for c := 0; c < src.Channels; c++ {
				sum := 0
				count := 0
				for dy := -radius; dy <= radius; dy++ {
					sy := y + dy
					if sy < 0 {
						sy = 0
					} else if sy >= src.Height {
						sy = src.Height - 1
					}
					for dx := -radius; dx <= radius; dx++ {
						sx := x + dx
						if sx < 0 {
							sx = 0
						} else if sx >= src.Width {
							sx = src.Width - 1
						}
						sum += int(src.Pix[src.Offset(sx, sy)+c])
						count++
					}
				}
				avg := float64(sum) / float64(count)
				v := inv*float64(src.Pix[o+c]) + alpha*avg
				dst.Pix[o+c] = clampByte(v)
			}
		}
	}
}

func applyBlockiness(dst, src *core.Frame, region core.Rect, strength float64) {
	const size = 8
	blend := 0.72 * strength
	for by := (region.Y / size) * size; by < region.Bottom(); by += size {
		for bx := (region.X / size) * size; bx < region.Right(); bx += size {
			cell := core.Rect{X: bx, Y: by, W: size, H: size}.Clamp(src.Width, src.Height)
			if cell.Empty() {
				continue
			}
			overlap, ok := core.Intersection(cell, region)
			if !ok {
				continue
			}
			var sums [3]int
			count := cell.W * cell.H
			for y := cell.Y; y < cell.Bottom(); y++ {
				for x := cell.X; x < cell.Right(); x++ {
					o := src.Offset(x, y)
					for c := 0; c < 3 && c < src.Channels; c++ {
						sums[c] += int(src.Pix[o+c])
					}
				}
			}
			var means [3]float64
			for c := 0; c < 3; c++ {
				means[c] = float64(sums[c]) / float64(count)
			}
			for y := overlap.Y; y < overlap.Bottom(); y++ {
				for x := overlap.X; x < overlap.Right(); x++ {
					o := src.Offset(x, y)
					for c := 0; c < src.Channels; c++ {
						v := (1-blend)*float64(src.Pix[o+c]) + blend*means[min(c, 2)]
						dst.Pix[o+c] = clampByte(v)
					}
				}
			}
		}
	}
}

func applyChroma(dst, src *core.Frame, region core.Rect, strength float64) {
	cbShift := strength * 34
	crShift := -strength * 28
	for y := region.Y; y < region.Bottom(); y++ {
		for x := region.X; x < region.Right(); x++ {
			o := src.Offset(x, y)
			r := float64(src.Pix[o])
			g := float64(src.Pix[o+1])
			b := float64(src.Pix[o+2])
			yy := 0.299*r + 0.587*g + 0.114*b
			cb := 128 - 0.168736*r - 0.331264*g + 0.5*b + cbShift
			cr := 128 + 0.5*r - 0.418688*g - 0.081312*b + crShift
			rr := yy + 1.402*(cr-128)
			gg := yy - 0.344136*(cb-128) - 0.714136*(cr-128)
			bb := yy + 1.772*(cb-128)
			dst.Pix[o] = clampByte(rr)
			dst.Pix[o+1] = clampByte(gg)
			dst.Pix[o+2] = clampByte(bb)
		}
	}
}

func applyOverexpose(dst, src *core.Frame, region core.Rect, strength float64) {
	boost := strength * 82
	desat := strength * 0.28
	for y := region.Y; y < region.Bottom(); y++ {
		for x := region.X; x < region.Right(); x++ {
			o := src.Offset(x, y)
			local := float64((x*13+y*7)%11) * strength * 0.8
			var vals [3]float64
			for c := 0; c < 3; c++ {
				vals[c] = float64(src.Pix[o+c]) + boost + local
			}
			luma := 0.299*vals[0] + 0.587*vals[1] + 0.114*vals[2]
			for c := 0; c < 3; c++ {
				vals[c] = (1-desat)*vals[c] + desat*luma
				dst.Pix[o+c] = softClip(vals[c])
			}
		}
	}
}

func deterministicGaussian(seed int64, step int64, x, y, c int) float64 {
	var total float64
	for k := int64(0); k < 4; k++ {
		h := mix64(uint64(seed) ^ uint64(step)*0x9e3779b97f4a7c15 ^
			uint64(x)*0xbf58476d1ce4e5b9 ^ uint64(y)*0x94d049bb133111eb ^
			uint64(c+1)*0xd6e8feb86659fd93 ^ uint64(k)*0x9e3779b185ebca87)
		u := float64(h>>11) / float64(uint64(1)<<53)
		total += u
	}
	return total - 2.0
}

func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

func HashSeed(seed int64) uint64 {
	return mix64(uint64(seed) ^ 0x9e3779b97f4a7c15)
}

func BitMix(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	return mix64(hi ^ lo)
}

func clampByte(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

func softClip(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	if v > 225 {
		v = 225 + (v-225)*0.45
	}
	return clampByte(v)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
