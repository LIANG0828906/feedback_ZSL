package kernel

import (
	"fmt"
	"math"
)

type WindowCoord struct {
	Index int64
	Start int64
	End   int64
}

func AttributionRule(widthMs, slideMs int64) string {
	if widthMs%slideMs == 0 {
		return AttributionInteger
	}
	return AttributionFractional
}

func WidthSlideRatio(widthMs, slideMs int64) float64 {
	return float64(widthMs) / float64(slideMs)
}

func SelectPath(widthMs, slideMs int64, policy PathPolicy) PathDecision {
	ratio := WidthSlideRatio(widthMs, slideMs)
	path := PathRescan
	if ratio > policy.IncrementalRatioThreshold {
		path = PathIncremental
	}
	return PathDecision{
		Ratio:     ratio,
		Threshold: policy.IncrementalRatioThreshold,
		Path:      path,
		Rule:      PathDecisionRule,
		Boundary:  policy.Boundary,
	}
}

func WindowsForTime(anchorMs, widthMs, slideMs, timeMs int64) ([]WindowCoord, error) {
	if widthMs <= 0 {
		return nil, NewError(CodeDefinitionNegativeWidth, "window width must be positive", map[string]any{"width_ms": widthMs})
	}
	if slideMs <= 0 {
		return nil, NewError(CodeDefinitionZeroStep, "slide must be positive", map[string]any{"slide_ms": slideMs})
	}
	if timeMs < anchorMs {
		return nil, nil
	}

	relative := timeMs - anchorMs
	maxIndex := floorDiv(relative, slideMs)
	var minIndex int64
	if widthMs%slideMs == 0 {
		multiple := widthMs / slideMs
		minIndex = maxIndex - multiple + 1
	} else {
		minIndex = floorDiv(relative-widthMs, slideMs) + 1
	}

	windows := make([]WindowCoord, 0, int(maxIndex-minIndex+1))
	for index := minIndex; index <= maxIndex; index++ {
		start := anchorMs + index*slideMs
		end := start + widthMs
		if timeMs >= start && timeMs < end {
			windows = append(windows, WindowCoord{Index: index, Start: start, End: end})
		}
	}
	return windows, nil
}

func WindowAt(anchorMs, widthMs, slideMs, index int64) WindowCoord {
	start := anchorMs + index*slideMs
	return WindowCoord{Index: index, Start: start, End: start + widthMs}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
	}
	return q
}

func ValidateFeature(def FeatureDef) error {
	ctx := map[string]any{"feature_id": def.ID}
	if def.ID == "" {
		return NewError(CodeInvalidArgument, "feature id is required", ctx)
	}
	if def.WidthMs < 0 {
		ctx["width_ms"] = def.WidthMs
		return NewError(CodeDefinitionNegativeWidth, "window width must not be negative", ctx)
	}
	if def.WidthMs == 0 {
		ctx["width_ms"] = def.WidthMs
		return NewError(CodeDefinitionNegativeWidth, "window width must be positive", ctx)
	}
	if def.SlideMs == 0 {
		ctx["slide_ms"] = def.SlideMs
		return NewError(CodeDefinitionZeroStep, "slide must not be zero", ctx)
	}
	if def.SlideMs < 0 {
		ctx["slide_ms"] = def.SlideMs
		return NewError(CodeDefinitionZeroStep, "slide must be positive", ctx)
	}
	if def.WidthMs < def.SlideMs {
		ctx["width_ms"] = def.WidthMs
		ctx["slide_ms"] = def.SlideMs
		return NewError(CodeDefinitionInvalidGeometry, "window width must be at least slide", ctx)
	}
	if def.AllowedLatenessMs < 0 {
		ctx["allowed_lateness_ms"] = def.AllowedLatenessMs
		return NewError(CodeDefinitionInvalidGeometry, "allowed lateness must not be negative", ctx)
	}
	if def.AnchorMs < 0 {
		ctx["anchor_ms"] = def.AnchorMs
		return NewError(CodeDefinitionInvalidGeometry, "anchor must be non-negative epoch milliseconds", ctx)
	}
	if len(def.Aggregations) == 0 {
		return NewError(CodeAggUnknownFunction, "at least one aggregation is required", ctx)
	}
	seen := map[AggFunc]bool{}
	for _, fn := range def.Aggregations {
		if _, err := ParseAggFunc(string(fn)); err != nil {
			return err
		}
		if seen[fn] {
			ctx["function"] = string(fn)
			return NewError(CodeAggDuplicateFunction, "aggregation function is duplicated", ctx)
		}
		seen[fn] = true
	}
	scale := def.ChannelWeightsPPM
	for channel, weight := range def.ChannelWeightsPPM {
		if weight < 0 || weight > 1_000_000 {
			return NewError(CodeAggWeightOutOfRange, "channel fixed-point weight is outside declared scale", map[string]any{
				"feature_id": def.ID,
				"channel":    channel,
				"weight_ppm": weight,
				"min_ppm":    0,
				"max_ppm":    1_000_000,
			})
		}
	}
	_ = scale
	return nil
}

func ValidateBudget(budget Budget) error {
	if budget.Workers <= 0 || budget.Workers > MaxWorkers {
		return NewError(CodeBudgetWorkersOutOfRange, "worker budget is out of declared range", map[string]any{
			"workers": budget.Workers,
			"min":     1,
			"max":     MaxWorkers,
		})
	}
	if budget.MaxWindowsPerFeature <= 0 || budget.MaxWindowsPerFeature > MaxWindowBuffer {
		return NewError(CodeBudgetBufferOutOfRange, "per-feature window buffer is out of declared range", map[string]any{
			"max_windows_per_feature": budget.MaxWindowsPerFeature,
			"min":                     1,
			"max":                     MaxWindowBuffer,
		})
	}
	return nil
}

const (
	MaxWorkers      = 64
	MaxWindowBuffer = 4096
)

func NearlyEqual(a, b, tolerance float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	diff := math.Abs(a - b)
	if diff <= tolerance {
		return true
	}
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return diff <= tolerance*scale
}

func CompareWindowResults(want, got []WindowResult, tolerance float64) error {
	if len(want) != len(got) {
		return fmt.Errorf("window count mismatch: want %d got %d", len(want), len(got))
	}
	for i := range want {
		w := want[i]
		g := got[i]
		if w.FeatureID != g.FeatureID || w.WindowID != g.WindowID {
			return fmt.Errorf("window %d identity mismatch: want %s/%s got %s/%s", i, w.FeatureID, w.WindowID, g.FeatureID, g.WindowID)
		}
		if w.StartMs != g.StartMs || w.EndMs != g.EndMs || w.EventCount != g.EventCount {
			return fmt.Errorf("window %s basis/count mismatch: want %d-%d count=%d got %d-%d count=%d", w.WindowID, w.StartMs, w.EndMs, w.EventCount, g.StartMs, g.EndMs, g.EventCount)
		}
		if len(w.Aggregations) != len(g.Aggregations) {
			return fmt.Errorf("window %s aggregation count mismatch", w.WindowID)
		}
		for j := range w.Aggregations {
			if w.Aggregations[j].Func != g.Aggregations[j].Func {
				return fmt.Errorf("window %s agg order mismatch at %d: want %s got %s", w.WindowID, j, w.Aggregations[j].Func, g.Aggregations[j].Func)
			}
			if !NearlyEqual(w.Aggregations[j].Value, g.Aggregations[j].Value, tolerance) {
				return fmt.Errorf("window %s agg %s mismatch: want %.12f got %.12f", w.WindowID, w.Aggregations[j].Func, w.Aggregations[j].Value, g.Aggregations[j].Value)
			}
		}
	}
	return nil
}
