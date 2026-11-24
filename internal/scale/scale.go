package scale

import (
	"fmt"
	"math"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/model"
)

const (
	ModeFine      = "fine"
	ModeCandidate = "candidate-priority"
)

// Decide is a pure function of declared input shape and budget. It never uses
// wall-clock timing, worker ids, or previous decisions.
func Decide(width, height, frameCount, budget int, preference string) (model.ScaleDecision, error) {
	if width <= 0 || height <= 0 || frameCount <= 0 {
		e := apperr.New(apperr.Meta, "input dimensions must be positive before scale selection")
		e.Field = "frame_count"
		return model.ScaleDecision{}, e
	}
	if budget < 1 {
		e := apperr.New(apperr.Para, "sampling budget must be positive")
		e.Field = "sampling_budget"
		e.Expected = ">=1"
		e.Actual = budget
		e.AvailableRange = "1..MaxInt"
		return model.ScaleDecision{}, e
	}
	framePixels := width * height
	frameUnits := int(math.Ceil(float64(framePixels) / 100000.0))
	if frameUnits < 1 {
		frameUnits = 1
	}
	effective := frameCount * frameUnits
	mode := ModeCandidate
	if effective <= budget {
		mode = ModeFine
	}
	switch preference {
	case "", "auto":
	case ModeFine, "exact":
		mode = ModeFine
	case ModeCandidate, "candidate":
		mode = ModeCandidate
	default:
		e := apperr.New(apperr.Para, "unknown verification mode")
		e.Field = "mode"
		e.Expected = "auto|fine|candidate"
		e.Actual = preference
		return model.ScaleDecision{}, e
	}
	stride := 1
	if mode == ModeCandidate {
		stride = int(math.Ceil(float64(effective) / float64(budget)))
		if stride < 1 {
			stride = 1
		}
	}
	downsample := 1
	if framePixels > 100000 {
		downsample = 2
	}
	screened := make([]int, 0, min(frameCount, budget+1))
	for i := 0; i < frameCount; i += stride {
		screened = append(screened, i)
	}
	if len(screened) < budget && (len(screened) == 0 || screened[len(screened)-1] != frameCount-1) {
		// When budget permits, include the terminal frame to close a trailing interval.
		screened = append(screened, frameCount-1)
	}
	decision := model.ScaleDecision{
		Mode:             mode,
		FrameCount:       frameCount,
		FramePixels:      framePixels,
		TotalPixels:      int64(framePixels) * int64(frameCount),
		FrameUnits:       frameUnits,
		EffectiveFrames:  effective,
		SamplingBudget:   budget,
		Stride:           stride,
		BlockDownsample:  downsample,
		ScreenFrameCount: len(screened),
		SkippedRanges:    model.RangesFromIndices(screened, frameCount, true),
		ScreenedFrames:   screened,
		ExactRefinement:  true,
	}
	if mode == ModeFine {
		decision.Stride = 1
		decision.ScreenFrameCount = frameCount
		decision.SkippedRanges = nil
		decision.ScreenedFrames = nil
		decision.Basis = fmt.Sprintf(
			"frame_pixels=%d, frame_units=%d, frames=%d, effective_frames=%d <= sampling_budget=%d; fine mode hashes and analyzes every frame",
			framePixels, frameUnits, frameCount, effective, budget)
	} else {
		decision.Basis = fmt.Sprintf(
			"frame_pixels=%d, frame_units=%d, frames=%d, effective_frames=%d > sampling_budget=%d; candidate-priority screening uses stride=%d and block_downsample=%d, followed by exact refinement of candidate neighborhoods",
			framePixels, frameUnits, frameCount, effective, budget, stride, downsample)
	}
	return decision, nil
}

func CheckIntervalAgreement(fine, candidate model.Interval, tolerance int) error {
	startDelta, endDelta := model.IntervalDelta(fine, candidate)
	if startDelta <= tolerance && endDelta <= tolerance {
		return nil
	}
	e := apperr.New(apperr.Scale, "fine and candidate-priority modes located different replacement intervals")
	e.Field = "replacement"
	e.Left = map[string]any{"mode": ModeFine, "start": fine.Start, "end": fine.End}
	e.Right = map[string]any{"mode": ModeCandidate, "start": candidate.Start, "end": candidate.End}
	e.Expected = fmt.Sprintf("start/end delta <= %d", tolerance)
	e.Actual = fmt.Sprintf("start_delta=%d, end_delta=%d", startDelta, endDelta)
	e.Details = map[string]any{
		"start_delta": startDelta,
		"end_delta":   endDelta,
		"tolerance":   tolerance,
	}
	return e
}
