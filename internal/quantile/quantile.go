package quantile

import (
	"math"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
)

func BuildHistogram(pix []uint8, bitDepth int) ([]int, error) {
	if bitDepth != 8 {
		return nil, errs.Para("bit_depth", bitDepth, [2]any{8, 8})
	}
	h := make([]int, 1<<bitDepth)
	for _, v := range pix {
		h[v]++
	}
	return h, nil
}

func Level(h []int, q float64) (int, error) {
	if q <= 0 || q >= 1 || math.IsNaN(q) {
		return 0, errs.Para("quantile", q, [2]any{"(0,1)", nil})
	}
	if err := model.ValidateHistogram(h, 8); err != nil {
		return 0, err
	}
	total := 0
	for _, n := range h {
		total += n
	}
	want := int(math.Ceil(q * float64(total)))
	if want < 1 {
		want = 1
	}
	cum := 0
	for level, n := range h {
		cum += n
		if cum >= want {
			return level, nil
		}
	}
	return len(h) - 1, nil
}

type Result struct {
	BGLevel   int
	FGLevel   int
	Threshold float64
}

func Estimate(h []int, bgQ, fgQ float64) (Result, error) {
	if bgQ <= 0 || bgQ >= 0.5 {
		return Result{}, errs.Para("bg_quantile", bgQ, [2]any{"(0,0.5)", nil})
	}
	if fgQ <= 0.5 || fgQ >= 1 {
		return Result{}, errs.Para("fg_quantile", fgQ, [2]any{"(0.5,1)", nil})
	}
	bg, err := Level(h, bgQ)
	if err != nil {
		return Result{}, err
	}
	fg, err := Level(h, fgQ)
	if err != nil {
		return Result{}, err
	}
	if fg < bg {
		bg, fg = fg, bg
	}
	return Result{BGLevel: bg, FGLevel: fg, Threshold: float64(bg+fg) / 2.0}, nil
}

// TightenedPair moves the background and foreground quantiles toward 0.5 in
// deterministic steps. The estimated class levels therefore move inward
// monotonically. The separation threshold is the midpoint and becomes stable
// once the discrete histogram levels stop changing.
func TightenedPair(bgQ, fgQ, step float64) (float64, float64) {
	bg := math.Min(0.49, bgQ+step)
	fg := math.Max(0.51, fgQ-step)
	return bg, fg
}
