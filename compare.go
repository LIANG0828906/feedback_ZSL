package dialoguecal

import (
	"fmt"
	"sort"
)

// CompareTruth checks the independently generated parse truth against the
// processor output. Features are exact; calibrated loudness is bounded by the
// project tolerance. The first mismatch in dialogue/frame order is reported.
func CompareTruth(truth []FrameTruth, results []IntervalResult, tolerance int64) error {
	observed := make(map[string]struct {
		feature    FrameFeature
		calibrated int64
	})
	for _, iv := range results {
		cal := make(map[int]int64, len(iv.Calibrations))
		for _, c := range iv.Calibrations {
			cal[c.Frame] = c.CalibratedLoudness
		}
		for _, f := range iv.Features {
			observed[fmt.Sprintf("%s/%08d", f.DialogueID, f.Frame)] = struct {
				feature    FrameFeature
				calibrated int64
			}{feature: f, calibrated: cal[f.Frame]}
		}
	}
	sortedTruth := append([]FrameTruth(nil), truth...)
	sort.Slice(sortedTruth, func(i, j int) bool {
		if sortedTruth[i].DialogueID != sortedTruth[j].DialogueID {
			return sortedTruth[i].DialogueID < sortedTruth[j].DialogueID
		}
		return sortedTruth[i].Frame < sortedTruth[j].Frame
	})
	for _, t := range sortedTruth {
		k := fmt.Sprintf("%s/%08d", t.DialogueID, t.Frame)
		o, ok := observed[k]
		if !ok {
			return &Error{
				Code: CodeDiff, SegmentID: t.SegmentID, Frame: t.Frame, HasFrame: true,
				FirstSegmentID: t.SegmentID, FirstFrame: t.Frame, HasFirstFrame: true,
				Message: "truth frame is missing from processed result",
			}
		}
		dev := o.calibrated - t.CalibratedLoudness
		if dev < 0 {
			dev = -dev
		}
		if o.feature.Energy != t.Energy || o.feature.ZeroCrossings != t.ZeroCrossings ||
			o.feature.TransientPeak != t.TransientPeak || o.feature.Weight != t.Weight ||
			o.feature.StartSample != t.StartSample || o.feature.EndSample != t.EndSample ||
			dev > tolerance {
			return &Error{
				Code: CodeDiff, SegmentID: t.SegmentID, Frame: t.Frame, HasFrame: true,
				FirstSegmentID: t.SegmentID, FirstFrame: t.Frame, HasFirstFrame: true,
				OperandA: o.feature.Energy, OperandB: t.Energy,
				Message: fmt.Sprintf(
					"truth mismatch: energy=%d/%d zcr=%d/%d peak=%d/%d weight=%d/%d loudness-dev=%d tolerance=%d",
					o.feature.Energy, t.Energy, o.feature.ZeroCrossings, t.ZeroCrossings,
					o.feature.TransientPeak, t.TransientPeak, o.feature.Weight, t.Weight, dev, tolerance),
			}
		}
	}
	return nil
}
