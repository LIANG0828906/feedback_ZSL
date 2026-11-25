package engine

import (
	"fmt"

	"uvenhance/internal/apperr"
	"uvenhance/internal/cache"
)

func CompareResults(a, b *RunResult) error {
	if a == nil || b == nil {
		return apperr.Diff("pipeline", 0, 0, "one comparison result is nil")
	}
	for _, spec := range cache.Stages {
		left, right := a.Stages[spec.ID], b.Stages[spec.ID]
		if left == nil || right == nil {
			return apperr.Diff(spec.ID, 0, 0, "stage missing on one path")
		}
		if left.Kind != right.Kind || left.Width != right.Width || left.Height != right.Height ||
			left.BitDepth != right.BitDepth || left.QuantMin != right.QuantMin || left.QuantMax != right.QuantMax {
			return apperr.Diff(spec.ID, 0, 0,
				fmt.Sprintf("stage metadata mismatch: left=%dx%d/%d/%d-%d right=%dx%d/%d/%d-%d",
					left.Width, left.Height, left.BitDepth, left.QuantMin, left.QuantMax,
					right.Width, right.Height, right.BitDepth, right.QuantMin, right.QuantMax))
		}
		switch spec.Kind {
		case cache.KindPlane:
			if left.PlaneData == nil || right.PlaneData == nil {
				return apperr.Diff(spec.ID, 0, 0, "plane payload missing")
			}
			for i := range left.PlaneData.Data {
				if left.PlaneData.Data[i] != right.PlaneData.Data[i] {
					return apperr.Diff(spec.ID, i%left.Width, i/left.Width,
						fmt.Sprintf("first plane sample mismatch: left=%d right=%d", left.PlaneData.Data[i], right.PlaneData.Data[i]))
				}
			}
		case cache.KindImage:
			if left.ImageData == nil || right.ImageData == nil {
				return apperr.Diff(spec.ID, 0, 0, "image payload missing")
			}
			for i, lp := range left.ImageData.Pixels {
				rp := right.ImageData.Pixels[i]
				if lp != rp {
					channel, lv, rv := "Y", lp.Y, rp.Y
					if lp.U != rp.U {
						channel, lv, rv = "U", lp.U, rp.U
					} else if lp.V != rp.V {
						channel, lv, rv = "V", lp.V, rp.V
					}
					return apperr.Diff(spec.ID, i%left.Width, i/left.Width,
						fmt.Sprintf("first interleaved pixel mismatch: channel=%s left=%d right=%d", channel, lv, rv))
				}
			}
		}
	}
	return nil
}

func CompareStages(a, b map[string]*cache.Output) error {
	return CompareResults(&RunResult{Stages: a}, &RunResult{Stages: b})
}
