package dialogue

import (
	"fmt"
	"math"
)

type CalibratedFrame struct {
	FrameIndex  int          `json:"frame_index"`
	Envelope    int64        `json:"envelope"`
	Truncations []Truncation `json:"truncations,omitempty"`
}

type SegmentCalibration struct {
	SegmentID          string            `json:"segment_id"`
	SessionGain        int64             `json:"session_gain"`
	GainAdjustment     int64             `json:"gain_adjustment"`
	AppliedGain        int64             `json:"applied_gain"`
	BaseLoudness       int64             `json:"base_loudness"`
	CalibratedLoudness int64             `json:"calibrated_loudness"`
	MinReachable       int64             `json:"min_reachable"`
	MaxReachable       int64             `json:"max_reachable"`
	Reached            bool              `json:"reached"`
	Limited            bool              `json:"limited"`
	Frames             []CalibratedFrame `json:"frames"`
	Truncations        []Truncation      `json:"truncations,omitempty"`
	EnvelopeDigest     string            `json:"envelope_digest"`
}

type CalibrationResult struct {
	TargetLoudness    int64                `json:"target_loudness"`
	Tolerance         int64                `json:"tolerance"`
	Segments          []SegmentCalibration `json:"segments"`
	ProjectLoudness   int64                `json:"project_loudness"`
	MinReachable      int64                `json:"min_reachable"`
	MaxReachable      int64                `json:"max_reachable"`
	Reached           bool                 `json:"reached"`
	LimitedSegmentIDs []string             `json:"limited_segment_ids,omitempty"`
}

// CalibrateEnvelopes derives each segment's adjustment from its measured
// envelope and clamps both the declared adjustment range and absolute gain range.
func CalibrateEnvelopes(project *Project, envelopes []*SegmentEnvelope) (*CalibrationResult, error) {
	if project == nil {
		return nil, newError(CodeSchema, "", "project", "project is nil")
	}
	byID := map[string]*SegmentEnvelope{}
	for _, envelope := range envelopes {
		byID[envelope.SegmentID] = envelope
	}
	result := &CalibrationResult{TargetLoudness: project.TargetLoudness, Tolerance: DefaultFrameTolerance}
	segments := project.Segments()
	var allCalibrated []int64
	var limited []string
	for _, segment := range segments {
		envelope, ok := byID[segment.ID]
		if !ok {
			return nil, newError(CodeSchema, project.ID, "segments."+segment.ID, "missing envelope measurement")
		}
		calibration, err := calibrateSegment(project, segment, envelope)
		if err != nil {
			return nil, err
		}
		result.Segments = append(result.Segments, calibration)
		for _, frame := range calibration.Frames {
			allCalibrated = append(allCalibrated, frame.Envelope)
		}
		if calibration.Limited {
			limited = append(limited, segment.ID)
		}
	}
	result.LimitedSegmentIDs = limited
	if len(allCalibrated) > 0 {
		var sum int64
		for _, value := range allCalibrated {
			sum += value
		}
		result.ProjectLoudness = sum / int64(len(allCalibrated))
	}
	if len(result.Segments) > 0 {
		result.MinReachable = result.Segments[0].MinReachable
		result.MaxReachable = result.Segments[0].MaxReachable
		for _, segment := range result.Segments[1:] {
			if segment.MinReachable > result.MinReachable {
				result.MinReachable = segment.MinReachable
			}
			if segment.MaxReachable < result.MaxReachable {
				result.MaxReachable = segment.MaxReachable
			}
		}
	}
	result.Reached = len(limited) == 0
	if len(limited) > 0 {
		return result, &Error{
			Code: CodeLoud, DialogueID: project.ID, Field: "target_loudness", Path: "target_loudness",
			Message:        "target loudness is unreachable within the declared gain bounds",
			TargetLoudness: project.TargetLoudness, MinReachable: result.MinReachable,
			MaxReachable: result.MaxReachable, LimitedIDs: limited,
			Values: []string{fmt.Sprintf("tolerance=%d", DefaultFrameTolerance)},
		}
	}
	return result, nil
}

func calibrateSegment(project *Project, segment Segment, envelope *SegmentEnvelope) (SegmentCalibration, error) {
	out := SegmentCalibration{
		SegmentID: segment.ID, SessionGain: segment.SessionGain,
		EnvelopeDigest: envelope.Digest(),
	}
	if len(envelope.Frames) == 0 {
		return out, newError(CodeSchema, project.ID, "segments."+segment.ID, "empty envelope")
	}
	var baseSum int64
	for _, frame := range envelope.Frames {
		baseSum += frame.Envelope
	}
	out.BaseLoudness = baseSum / int64(len(envelope.Frames))
	if baseSum%int64(len(envelope.Frames)) != 0 {
		out.Truncations = append(out.Truncations, Truncation{
			Location: segment.ID + ".base_loudness", Field: "base_loudness", Operation: "sum/frame_count",
			OperandA: baseSum, OperandB: int64(len(envelope.Frames)), RoundedValue: out.BaseLoudness,
			Remainder: baseSum % int64(len(envelope.Frames)), Residue: baseSum % int64(len(envelope.Frames)), Mode: "toward-zero",
		})
	}
	minGain := segment.SessionGain - project.GainLimit
	maxGain := segment.SessionGain + project.GainLimit
	if minGain < project.GainMin {
		minGain = project.GainMin
	}
	if maxGain > project.GainMax {
		maxGain = project.GainMax
	}
	if minGain < 0 {
		minGain = 0
	}
	if maxGain < 0 {
		maxGain = 0
	}
	baseAtMin := mulDivFloor(out.BaseLoudness, minGain, InternalScale)
	baseAtMax := mulDivFloor(out.BaseLoudness, maxGain, InternalScale)
	out.MinReachable = baseAtMin
	out.MaxReachable = baseAtMax
	target := project.TargetLoudness
	out.Limited = out.BaseLoudness == 0 ||
		target < out.MinReachable-DefaultFrameTolerance ||
		target > out.MaxReachable+DefaultFrameTolerance

	requiredGain := segment.SessionGain
	if out.BaseLoudness > 0 {
		rawTargetGain, remainder, residue, err := roundMulDiv(target, InternalScale, out.BaseLoudness, "required_gain", target, out.BaseLoudness)
		if err != nil {
			return out, err
		}
		if remainder != 0 {
			out.Truncations = append(out.Truncations, Truncation{
				Location: segment.ID + ".required_gain", Field: "required_gain", Operation: "*10000/base_loudness",
				OperandA: target, OperandB: out.BaseLoudness, RoundedValue: rawTargetGain,
				Remainder: remainder, Residue: residue, Mode: string(RoundHalfAwayFromZero),
			})
		}
		requiredGain = rawTargetGain
	}
	adjustment := requiredGain - segment.SessionGain
	if adjustment > project.GainLimit {
		adjustment = project.GainLimit
	}
	if adjustment < -project.GainLimit {
		adjustment = -project.GainLimit
	}
	applied := segment.SessionGain + adjustment
	if applied < project.GainMin {
		applied = project.GainMin
		adjustment = applied - segment.SessionGain
	}
	if applied > project.GainMax {
		applied = project.GainMax
		adjustment = applied - segment.SessionGain
	}
	if segment.SessionGain+adjustment > maxGain {
		adjustment = maxGain - segment.SessionGain
	}
	if segment.SessionGain+adjustment < minGain {
		adjustment = minGain - segment.SessionGain
	}
	out.GainAdjustment = adjustment
	out.AppliedGain = segment.SessionGain + adjustment

	var calibratedSum int64
	for _, frame := range envelope.Frames {
		value, _, residue, err := roundMulDiv(frame.Envelope, out.AppliedGain, InternalScale, "calibrated_envelope", frame.Envelope, out.AppliedGain)
		if err != nil {
			return out, err
		}
		var traces []Truncation
		if frame.Envelope*out.AppliedGain%InternalScale != 0 {
			traces = append(traces, Truncation{
				Location: fmt.Sprintf("%s.frame[%d].calibrate", segment.ID, frame.FrameIndex), Field: "calibrated_envelope",
				Operation: "*applied_gain/10000", OperandA: frame.Envelope, OperandB: out.AppliedGain,
				RoundedValue: value, Remainder: frame.Envelope * out.AppliedGain % InternalScale,
				Residue: residue, Mode: string(RoundHalfAwayFromZero),
			})
		}
		out.Frames = append(out.Frames, CalibratedFrame{FrameIndex: frame.FrameIndex, Envelope: value, Truncations: traces})
		out.Truncations = append(out.Truncations, traces...)
		calibratedSum += value
	}
	out.CalibratedLoudness = calibratedSum / int64(len(out.Frames))
	if calibratedSum%int64(len(out.Frames)) != 0 {
		out.Truncations = append(out.Truncations, Truncation{
			Location: segment.ID + ".calibrated_loudness", Field: "calibrated_loudness", Operation: "sum/frame_count",
			OperandA: calibratedSum, OperandB: int64(len(out.Frames)), RoundedValue: out.CalibratedLoudness,
			Remainder: calibratedSum % int64(len(out.Frames)), Residue: calibratedSum % int64(len(out.Frames)), Mode: "toward-zero",
		})
	}
	out.Reached = abs64(out.CalibratedLoudness-target) <= DefaultFrameTolerance
	if !out.Reached {
		out.Limited = true
	}
	return out, nil
}

func mulDivFloor(value, mul, div int64) int64 {
	if div == 0 {
		return 0
	}
	return value * mul / div
}

func abs64(value int64) int64 {
	if value < 0 {
		if value == math.MinInt64 {
			return math.MaxInt64
		}
		return -value
	}
	return value
}
