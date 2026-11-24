package convert

import (
	"fmt"

	"texlab/internal/apperr"
	"texlab/internal/model"
)

type Result struct {
	Image     model.Image
	Layout    string
	Labels    []string
	Operation string
}

func ValidateParams(in model.Image, e model.Entry) error {
	if in.Width <= 0 || in.Height <= 0 || len(in.Pixels) != in.Width*in.Height*4 {
		return apperr.At(apperr.EPara, e.Index, "source", "source image buffer is inconsistent")
	}
	if e.TargetDepth != 8 && e.TargetDepth != 16 {
		return apperr.At(apperr.EPara, e.Index, "target_depth", "target depth must be 8 or 16")
	}
	if e.TargetChannels < 1 || e.TargetChannels > 4 {
		return apperr.At(apperr.EPara, e.Index, "target_channels", "target channels must be between 1 and 4")
	}
	if len(e.Permutation) != e.TargetChannels {
		return apperr.At(apperr.EPara, e.Index, "permutation", "permutation length must equal target channels")
	}
	if e.Alpha != "straight" && e.Alpha != "premultiplied" {
		return apperr.At(apperr.EPara, e.Index, "alpha", "alpha must be straight or premultiplied")
	}
	if e.TargetChannels < 4 && e.Alpha == "premultiplied" {
		return apperr.At(apperr.EPara, e.Index, "alpha", "premultiplied output requires alpha")
	}
	if e.Quantize != "floor" && e.Quantize != "ceil" && e.Quantize != "round" {
		return apperr.At(apperr.EPara, e.Index, "quantize", "quantize must be floor, ceil, or round")
	}
	fill := map[int]model.FillSpec{}
	for _, f := range e.Fill {
		if f.TargetIndex < 0 || f.TargetIndex >= e.TargetChannels {
			return apperr.At(apperr.EPara, e.Index, "fill.target_index", "fill target index out of range")
		}
		fill[f.TargetIndex] = f
	}
	used := map[int]bool{}
	sourceLimit := in.Channels
	if sourceLimit <= 0 {
		sourceLimit = 4
	}
	for _, src := range e.Permutation {
		if src < -1 || src >= sourceLimit {
			return apperr.At(apperr.EPara, e.Index, "permutation", "channel index %d outside 0..%d", src, sourceLimit-1)
		}
		if src >= 0 {
			if used[src] {
				return apperr.At(apperr.EPara, e.Index, "permutation", "channel %d repeated", src)
			}
			used[src] = true
		} else if _, ok := fill[len(used)]; !ok {
			// The precise fill slot is validated below; this branch keeps
			// malformed add-channel specs from reaching pixel conversion.
		}
	}
	for slot, src := range e.Permutation {
		if src == -1 {
			f, ok := fill[slot]
			if !ok {
				return apperr.At(apperr.EPara, e.Index, fmt.Sprintf("fill[%d]", slot), "missing fill for target slot")
			}
			switch f.Source {
			case "constant":
				maxValue := 255
				if e.TargetDepth == 16 {
					maxValue = 65535
				}
				if f.Value < 0 || f.Value > maxValue {
					return apperr.At(apperr.EPara, e.Index, "fill.value", "constant fill out of range")
				}
			case "zero", "one", "alpha":
			case "channel":
				if f.SourceChannel < 0 || f.SourceChannel >= sourceLimit {
					return apperr.At(apperr.EPara, e.Index, "fill.source_channel", "source channel out of range")
				}
			default:
				return apperr.At(apperr.EPara, e.Index, "fill.source", "unknown fill source")
			}
		}
	}
	return nil
}

func Transform(in model.Image, e model.Entry, coverage []model.Rect) (Result, error) {
	return transform(in, e, coverage, 0)
}

// TransformLimited keeps per-thread conversion scratch to maxPixels by
// splitting coverage into horizontal bands. The resulting segment buffer is
// still emitted atomically by the caller.
func TransformLimited(in model.Image, e model.Entry, coverage []model.Rect, maxPixels int) (Result, error) {
	return transform(in, e, coverage, maxPixels)
}

func transform(in model.Image, e model.Entry, coverage []model.Rect, maxPixels int) (Result, error) {
	if err := ValidateParams(in, e); err != nil {
		return Result{}, err
	}
	targetMax := maxValue(e.TargetDepth)
	out := in.Clone()
	out.Depth = e.TargetDepth
	out.Channels = e.TargetChannels
	out.Alpha = e.Alpha

	fill := map[int]model.FillSpec{}
	for _, f := range e.Fill {
		fill[f.TargetIndex] = f
	}
	labels := make([]string, e.TargetChannels)
	for slot, src := range e.Permutation {
		if src >= 0 {
			labels[slot] = string("RGBA"[src])
		} else {
			labels[slot] = fill[slot].Channel
		}
	}
	out.Layout = ""
	for _, label := range labels {
		out.Layout += label
	}
	alphaSlot := -1
	rgbSlots := [3]int{-1, -1, -1}
	for slot, label := range labels {
		switch label {
		case "A":
			alphaSlot = slot
		case "R":
			rgbSlots[0] = slot
		case "G":
			rgbSlots[1] = slot
		case "B":
			rgbSlots[2] = slot
		}
	}
	if e.Alpha == "premultiplied" && alphaSlot < 0 {
		return Result{}, apperr.At(apperr.EPara, e.Index, "alpha", "premultiplied output has no alpha channel")
	}

	for _, rect := range splitCoverage(coverage, maxPixels) {
		x0 := max(0, rect.X)
		y0 := max(0, rect.Y)
		x1 := min(in.Width, rect.X+rect.W)
		y1 := min(in.Height, rect.Y+rect.H)
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				src := in.SemanticPixel(x, y)
				var slots [4]uint16
				for slot, srcIndex := range e.Permutation {
					if srcIndex >= 0 {
						slots[slot] = Quantize(src[srcIndex], in.Depth, e.TargetDepth, e.Quantize)
						continue
					}
					spec := fill[slot]
					switch spec.Source {
					case "constant":
						slots[slot] = uint16(spec.Value)
					case "zero":
						slots[slot] = 0
					case "one":
						slots[slot] = uint16(targetMax)
					case "alpha":
						if in.Channels >= 4 {
							slots[slot] = Quantize(src[3], in.Depth, e.TargetDepth, e.Quantize)
						} else {
							slots[slot] = uint16(targetMax)
						}
					case "channel":
						slots[slot] = Quantize(src[spec.SourceChannel], in.Depth, e.TargetDepth, e.Quantize)
					}
				}
				if e.Alpha != in.Alpha {
					alpha := uint16(targetMax)
					if alphaSlot >= 0 {
						alpha = slots[alphaSlot]
					}
					for _, slot := range rgbSlots {
						if slot < 0 {
							continue
						}
						if e.Alpha == "premultiplied" {
							slots[slot] = Premultiply(slots[slot], alpha, e.TargetDepth, e.Quantize)
						} else if in.Alpha == "premultiplied" {
							slots[slot] = Unpremultiply(slots[slot], alpha, e.TargetDepth, e.Quantize)
						}
					}
				}
				i := (y*in.Width + x) * 4
				for slot := 0; slot < 4; slot++ {
					if slot < e.TargetChannels {
						out.Pixels[i+slot] = slots[slot]
					} else {
						out.Pixels[i+slot] = 0
					}
				}
			}
		}
	}
	return Result{Image: out, Layout: out.Layout, Labels: labels, Operation: "channel_map+depth_quantize+alpha"}, nil
}

func splitCoverage(coverage []model.Rect, maxPixels int) []model.Rect {
	if maxPixels <= 0 {
		return coverage
	}
	out := make([]model.Rect, 0, len(coverage))
	for _, r := range coverage {
		if r.W <= 0 || r.H <= 0 || r.Area() <= maxPixels {
			out = append(out, r)
			continue
		}
		if r.W > maxPixels {
			for y := r.Y; y < r.Y+r.H; y++ {
				for x := r.X; x < r.X+r.W; x += maxPixels {
					w := min(maxPixels, r.X+r.W-x)
					out = append(out, model.Rect{ID: r.ID, X: x, Y: y, W: w, H: 1})
				}
			}
			continue
		}
		rows := maxPixels / r.W
		for y := r.Y; y < r.Y+r.H; y += rows {
			h := min(rows, r.Y+r.H-y)
			out = append(out, model.Rect{ID: r.ID, X: r.X, Y: y, W: r.W, H: h})
		}
	}
	return out
}

func Quantize(v uint16, fromDepth, toDepth int, mode string) uint16 {
	if fromDepth == toDepth {
		return v
	}
	fromMax := uint64(maxValue(fromDepth))
	toMax := uint64(maxValue(toDepth))
	num := uint64(v) * toMax
	if fromDepth == 8 && toDepth == 16 {
		// 65535 is exactly 257 * 255; this branch also keeps the operation
		// stable if the constants change in a future schema.
		return uint16(num / fromMax)
	}
	var q uint64
	switch mode {
	case "ceil":
		q = (num + fromMax - 1) / fromMax
	case "round":
		q = (num + fromMax/2) / fromMax
	default:
		q = num / fromMax
	}
	if q > toMax {
		q = toMax
	}
	return uint16(q)
}

// Premultiply uses the declared quantization direction. At alpha zero the
// straight RGB payload is retained so a zero-alpha step remains reversible;
// this fallback is part of the TexLab alpha convention.
func Premultiply(c, a uint16, depth int, mode string) uint16 {
	if a == 0 {
		return c
	}
	maxValue := uint64(maxValue(depth))
	num := uint64(c) * uint64(a)
	switch mode {
	case "ceil":
		return uint16(min64(maxValue, (num+maxValue-1)/maxValue))
	case "round":
		return uint16(min64(maxValue, (num+maxValue/2)/maxValue))
	default:
		return uint16(num / maxValue)
	}
}

func Unpremultiply(c, a uint16, depth int, mode string) uint16 {
	if a == 0 {
		return c
	}
	maxValue := uint64(maxValue(depth))
	num := uint64(c) * maxValue
	var q uint64
	switch mode {
	case "ceil":
		q = (num + uint64(a) - 1) / uint64(a)
	case "round":
		q = (num + uint64(a)/2) / uint64(a)
	default:
		q = num / uint64(a)
	}
	return uint16(min64(maxValue, q))
}

// AlphaStepBound is the integer error bound for a premultiply followed by an
// unpremultiply at the given alpha. It is used by tests and reports.
func AlphaStepBound(a uint16, depth int) int {
	if a == 0 {
		return 0
	}
	step := (maxValue(depth) + int(a) - 1) / int(a)
	if step < 1 {
		return 1
	}
	return step
}

func maxValue(depth int) int {
	if depth == 8 {
		return 255
	}
	return 65535
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
