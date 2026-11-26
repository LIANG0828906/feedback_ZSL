package dialoguecal

import "fmt"

const (
	LoudnessFloor   int64 = -600000
	LoudnessCeiling int64 = 0
	GainFloor       int64 = -240000
	GainCeiling     int64 = 240000
)

// EnergyLoudness10K maps mean frame power to 1/10000 dBFS using an integer
// log2 approximation. A full-scale square wave has mean power 1<<30 and maps
// to 0 dB; silence is clamped to the declared floor.
func EnergyLoudness10K(energy int64, sampleCount int) int64 {
	if energy <= 0 || sampleCount <= 0 {
		return LoudnessFloor
	}
	meanSquare := energy / int64(sampleCount)
	if meanSquare <= 0 {
		return LoudnessFloor
	}
	// Full-scale power is 32768^2 == 1<<30.
	logQ16, _ := log2Q16(meanSquare)
	logQ16 -= 30 << 16
	num, _ := checkedMul(logQ16, 30103, "loudness.energy")
	db, _ := divRound(num, 65536, RoundHalfAway)
	if db < LoudnessFloor {
		db = LoudnessFloor
	}
	if db > LoudnessCeiling {
		db = LoudnessCeiling
	}
	return db
}

func weightedMeanLoudness(values, weights []int64) int64 {
	var sum int64
	var total int64
	for i := range values {
		w := weights[i]
		if w <= 0 {
			continue
		}
		sum += values[i] * w
		total += w
	}
	if total == 0 {
		return LoudnessFloor
	}
	q, _ := divRound(sum, total, RoundHalfAway)
	return q
}

func FrameBaseLoudness(feature FrameFeature, sessionGain, segmentGain int64) int64 {
	raw := EnergyLoudness10K(feature.Energy, feature.SampleCount)
	return raw + sessionGain + segmentGain
}

// MeasureFeatures returns the duration-weighted segment loudness after the
// declared session and segment gains have been applied.
func MeasureFeatures(p *Project, seg Segment, features []FrameFeature) (int64, []int64, error) {
	if p == nil {
		return 0, nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	if len(features) == 0 {
		return 0, nil, &Error{Code: CodeCal, Field: "frames", SegmentID: seg.ID, Message: "segment has no frames"}
	}
	base := make([]int64, len(features))
	weights := make([]int64, len(features))
	for i := range features {
		base[i] = FrameBaseLoudness(features[i], p.SessionGain, seg.Gain)
		weights[i] = int64(features[i].Weight)
		if weights[i] <= 0 {
			weights[i] = DefaultFrameWeight
		}
	}
	return weightedMeanLoudness(base, weights), base, nil
}

// CalibrateFeatures solves an additive gain adjustment for one segment. The
// target is checked against the declared final-gain ceiling before any result
// is accepted; a clipped value is never substituted for an unreachable target.
func CalibrateFeatures(p *Project, seg Segment, features []FrameFeature) ([]FrameCalibration, int64, int64, int64, error) {
	if p == nil {
		return nil, 0, 0, 0, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	if len(features) == 0 {
		return nil, 0, 0, 0, &Error{Code: CodeCal, Field: "frames", SegmentID: seg.ID, Message: "segment has no frames"}
	}
	measured, base, err := MeasureFeatures(p, seg, features)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	requested := p.TargetLoudness - measured
	maxAdjustment := p.GainUpperBound - seg.Gain
	if maxAdjustment < 0 {
		return nil, measured, requested, 0, &Error{
			Code: CodeCal, Field: "segments.gain", SegmentID: seg.ID,
			OperandA: seg.Gain, OperandB: p.GainUpperBound,
			Message: "declared segment gain already exceeds gain ceiling",
		}
	}
	reachable := measured + maxAdjustment
	if requested > maxAdjustment {
		frame := features[0].Frame
		return nil, measured, requested, maxAdjustment, &Error{
			Code: CodeLoud, Field: "targetLoudness", SegmentID: seg.ID,
			Frame: frame, HasFrame: true,
			Target: p.TargetLoudness, ReachableUpper: reachable,
			OperandA: requested, OperandB: maxAdjustment,
			Message: "target loudness is unreachable within declared gain ceiling",
		}
	}
	cal := make([]FrameCalibration, len(features))
	for i := range features {
		cal[i] = FrameCalibration{
			DialogueID: features[i].DialogueID, SegmentID: features[i].SegmentID,
			Frame: features[i].Frame, Weight: features[i].Weight,
			RawLoudness: base[i], GainAdjustment: requested,
			CalibratedLoudness: base[i] + requested,
		}
	}
	return cal, measured, requested, requested, nil
}

// TruthDeviation returns the maximum absolute per-frame deviation for the
// requested field. It is used by the generator acceptance path; it never feeds
// generated truth back into the processor.
func TruthDeviation(truth []FrameTruth, results []IntervalResult) (int64, error) {
	observed := make(map[string]int64, len(truth))
	for _, iv := range results {
		for _, c := range iv.Calibrations {
			observed[fmt.Sprintf("%s/%d", c.DialogueID, c.Frame)] = c.CalibratedLoudness
		}
	}
	var maxDev int64
	for _, t := range truth {
		v, ok := observed[fmt.Sprintf("%s/%d", t.DialogueID, t.Frame)]
		if !ok {
			return 0, &Error{
				Code: CodeDiff, SegmentID: t.SegmentID, Frame: t.Frame, HasFrame: true,
				FirstSegmentID: t.SegmentID, FirstFrame: t.Frame, HasFirstFrame: true,
				Message: "truth frame was not produced",
			}
		}
		dev := v - t.CalibratedLoudness
		if dev < 0 {
			dev = -dev
		}
		if dev > maxDev {
			maxDev = dev
		}
	}
	return maxDev, nil
}
