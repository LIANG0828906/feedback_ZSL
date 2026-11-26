package engine

import (
	"fmt"
	"sort"

	"dialogueaudio/audioerr"
	"dialogueaudio/fixed"
	"dialogueaudio/model"
)

// CalibratedFrame records the integer gain-and-remeasure path.
type CalibratedFrame struct {
	FrameIndex        int           `json:"frame_index"`
	BeforeEnvelopeQ16 int64         `json:"before_envelope_q16"`
	AfterEnergy       int64         `json:"after_energy"`
	AfterEnvelopeQ16  int64         `json:"after_envelope_q16"`
	AdjustmentDBQ16   int64         `json:"adjustment_db_q16"`
	Traces            []fixed.Trace `json:"traces,omitempty"`
}

// ClipCalibration is the frozen per-clip calibration result.
type ClipCalibration struct {
	ClipID                  string            `json:"clip_id"`
	BeforeLoudnessQ16       int64             `json:"before_loudness_q16"`
	RequiredAdjustmentDBQ16 int64             `json:"required_adjustment_db_q16"`
	AppliedAdjustmentDBQ16  int64             `json:"applied_adjustment_db_q16"`
	ClampedDBQ16            int64             `json:"clamped_db_q16"`
	AdjustmentGainQ16       int64             `json:"adjustment_gain_q16"`
	AfterLoudnessQ16        int64             `json:"after_loudness_q16"`
	TargetDeviationQ16      int64             `json:"target_deviation_q16"`
	WithinTolerance         bool              `json:"within_tolerance"`
	Frames                  []CalibratedFrame `json:"frames"`
	Traces                  []fixed.Trace     `json:"traces,omitempty"`
}

// CalibrationResult is deterministic for a project and its envelopes.
type CalibrationResult struct {
	TargetLoudnessQ16  int64             `json:"target_loudness_q16"`
	MaxAdjustmentDBQ16 int64             `json:"max_adjustment_db_q16"`
	ToleranceQ16       int64             `json:"tolerance_q16"`
	Clips              []ClipCalibration `json:"clips"`
}

// CalibrateProject solves one required adjustment per clip from its measured
// weighted segment energy and the session gain already embedded in it. The
// declared bound is symmetric (+/- max adjustment).
func CalibrateProject(project model.Project, envelopes map[string]ClipEnvelope, toleranceQ16 int64) (CalibrationResult, error) {
	if toleranceQ16 < 0 {
		return CalibrationResult{}, audioerr.Schema("calibration.tolerance_q16", "must not be negative")
	}
	result := CalibrationResult{
		TargetLoudnessQ16:  project.TargetLoudnessQ16,
		MaxAdjustmentDBQ16: project.MaxAdjustmentDBQ16,
		ToleranceQ16:       toleranceQ16,
		Clips:              make([]ClipCalibration, 0, len(project.Clips)),
	}
	clips := append([]model.Clip(nil), project.Clips...)
	sort.Slice(clips, func(i, j int) bool { return clips[i].ID < clips[j].ID })
	if !project.CalibrationEnabled {
		for _, clip := range clips {
			env := envelopes[clip.ID]
			calibrated := ClipCalibration{
				ClipID: clip.ID, BeforeLoudnessQ16: env.SegmentEnvelopeQ16,
				AfterLoudnessQ16: env.SegmentEnvelopeQ16, AdjustmentGainQ16: fixed.Q16,
				Frames: make([]CalibratedFrame, 0, len(env.Frames)),
			}
			for _, frame := range env.Frames {
				calibrated.Frames = append(calibrated.Frames, CalibratedFrame{
					FrameIndex: frame.FrameIndex, BeforeEnvelopeQ16: frame.EnvelopeQ16,
					AfterEnergy: frame.WeightedEnergy, AfterEnvelopeQ16: frame.EnvelopeQ16,
				})
			}
			calibrated.TargetDeviationQ16 = AbsInt64(calibrated.AfterLoudnessQ16 - project.TargetLoudnessQ16)
			calibrated.WithinTolerance = calibrated.TargetDeviationQ16 <= toleranceQ16
			result.Clips = append(result.Clips, calibrated)
		}
		return result, nil
	}

	var firstLimited string
	var firstMeasured int64
	var minUpper int64
	var maxLower int64
	for i, clip := range clips {
		env, ok := envelopes[clip.ID]
		if !ok {
			return result, audioerr.Schema("clips."+clip.ID+".envelope", "envelope is missing")
		}
		required, err := fixed.CheckedAdd(project.TargetLoudnessQ16, -env.SegmentEnvelopeQ16,
			"clips."+clip.ID+".adjustment_db_q16")
		if err != nil {
			return result, err
		}
		upper, err := fixed.CheckedAdd(env.SegmentEnvelopeQ16, project.MaxAdjustmentDBQ16,
			"clips."+clip.ID+".reachable_upper")
		if err != nil {
			return result, err
		}
		lower, err := fixed.CheckedAdd(env.SegmentEnvelopeQ16, -project.MaxAdjustmentDBQ16,
			"clips."+clip.ID+".reachable_lower")
		if err != nil {
			return result, err
		}
		if i == 0 || upper < minUpper {
			minUpper = upper
		}
		if i == 0 || lower > maxLower {
			maxLower = lower
		}
		if firstLimited == "" && (required > project.MaxAdjustmentDBQ16 || required < -project.MaxAdjustmentDBQ16) {
			firstLimited = clip.ID
			firstMeasured = env.SegmentEnvelopeQ16
		}
	}
	if firstLimited != "" {
		reachableUpper := firstMeasured + project.MaxAdjustmentDBQ16
		reachableLower := firstMeasured - project.MaxAdjustmentDBQ16
		return result, audioerr.New(audioerr.CodeLoudness, "calibration.target_loudness",
			"target loudness is unreachable within all clip adjustment bounds").
			WithValues(map[string]any{
				"target_q16":          project.TargetLoudnessQ16,
				"reachable_upper_q16": reachableUpper,
				"reachable_lower_q16": reachableLower,
				"global_upper_q16":    minUpper,
				"global_lower_q16":    maxLower,
				"first_limited_clip":  firstLimited,
			})
	}

	for _, clip := range clips {
		env := envelopes[clip.ID]
		required, err := fixed.CheckedAdd(project.TargetLoudnessQ16, -env.SegmentEnvelopeQ16,
			"clips."+clip.ID+".adjustment_db_q16")
		if err != nil {
			return result, err
		}
		adjustGain, gainTrace, err := fixed.DBToGainQ16(required, "clips."+clip.ID+".adjustment_gain_q16")
		if err != nil {
			return result, err
		}
		calibrated := ClipCalibration{
			ClipID:                  clip.ID,
			BeforeLoudnessQ16:       env.SegmentEnvelopeQ16,
			RequiredAdjustmentDBQ16: required,
			AppliedAdjustmentDBQ16:  required,
			ClampedDBQ16:            0,
			AdjustmentGainQ16:       adjustGain,
			Frames:                  make([]CalibratedFrame, 0, len(env.Frames)),
			Traces:                  []fixed.Trace{gainTrace},
		}
		var energySum int64
		for _, frame := range env.Frames {
			afterEnergy, err := fixed.ApplyGainToEnergy(frame.WeightedEnergy, adjustGain,
				fmt.Sprintf("clip.%s.frames[%d].calibrated_energy", clip.ID, frame.FrameIndex))
			if err != nil {
				return result, err
			}
			afterDB, dbTrace, err := fixed.EnergyToDBQ16(afterEnergy,
				fmt.Sprintf("clip.%s.frames[%d].calibrated_envelope_q16", clip.ID, frame.FrameIndex))
			if err != nil {
				return result, err
			}
			calibrated.Frames = append(calibrated.Frames, CalibratedFrame{
				FrameIndex:        frame.FrameIndex,
				BeforeEnvelopeQ16: frame.EnvelopeQ16,
				AfterEnergy:       afterEnergy,
				AfterEnvelopeQ16:  afterDB,
				AdjustmentDBQ16:   required,
				Traces: []fixed.Trace{
					{
						Operation: "apply_calibration_gain_twice", Field: fmt.Sprintf("clip.%s.frames[%d].calibrated_energy", clip.ID, frame.FrameIndex),
						Input: frame.WeightedEnergy, Output: afterEnergy, Rounding: fixed.RoundTrunc,
						Truncated: true, TruncationAt: "muldiv_q16",
					},
					dbTrace,
				},
			})
			energySum, err = fixed.CheckedAdd(energySum, afterEnergy,
				fmt.Sprintf("clip.%s.calibrated_summary", clip.ID))
			if err != nil {
				return result, err
			}
		}
		var tr fixed.Trace
		afterEnergy, tr, err := fixed.Rescale(energySum, int64(len(calibrated.Frames)), 1, fixed.RoundTrunc,
			"clip."+clip.ID+".calibrated_segment_energy")
		if err != nil {
			return result, err
		}
		calibrated.Traces = append(calibrated.Traces, tr)
		calibrated.AfterLoudnessQ16, tr, err = fixed.EnergyToDBQ16(afterEnergy,
			"clip."+clip.ID+".calibrated_loudness_q16")
		if err != nil {
			return result, err
		}
		calibrated.Traces = append(calibrated.Traces, tr)
		calibrated.TargetDeviationQ16 = AbsInt64(calibrated.AfterLoudnessQ16 - project.TargetLoudnessQ16)
		calibrated.WithinTolerance = calibrated.TargetDeviationQ16 <= toleranceQ16
		result.Clips = append(result.Clips, calibrated)
	}
	return result, nil
}

// CalibrationByClip indexes a result for comparisons.
func CalibrationByClip(result CalibrationResult) map[string]ClipCalibration {
	index := make(map[string]ClipCalibration, len(result.Clips))
	for _, clip := range result.Clips {
		index[clip.ClipID] = clip
	}
	return index
}
