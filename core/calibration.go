package dialoguecal

import "fmt"

const ActiveRMSThreshold Q15 = 160

type calibrationCarry struct {
	state            CalibrationState
	smoothCorrection Q8
	stableFrames     int
	lastLoudnessDB   Q8
	hasActive        bool
}

func newCalibrationCarry() calibrationCarry {
	return calibrationCarry{state: StateUncalibrated}
}

func floorAverageQ8(a, b Q8) Q8 {
	sum := int64(a) + int64(b)
	return Q8(sum >> 1)
}

func addClampedQ8(a Q8, b Q8) Q8 {
	v := int64(a) + int64(b)
	if v > Int32Max {
		return Q8(Int32Max)
	}
	if v < Int32Min {
		return Q8(Int32Min)
	}
	return Q8(v)
}

// CalibrateFrame applies segment gain, the declared per-frame energy cap,
// fixed-point smoothing, and deterministic state transitions.
func CalibrateFrame(p *Project, seg *Segment, frame Frame, features Features, prev *calibrationCarry) (Calibration, calibrationCarry) {
	saturations := []string{}
	raw := features.RMSQ15
	gained, sat := MulQ15(raw, seg.GainQ15)
	if sat != nil {
		saturations = append(saturations, sat.Operation)
	}

	energy := gained
	limited := false
	limitSource := ""
	if gained > p.EnergyCapQ15 {
		energy = p.EnergyCapQ15
		limited = true
		limitSource = fmt.Sprintf("segment:%s frame:%d rule:FRAME_ENERGY_CAP raw_q15:%d gained_q15:%d cap_q15:%d", seg.ID, frame.GlobalIndex, raw, gained, p.EnergyCapQ15)
	}

	loudnessDB := LinearToDBQ8(energy)
	weight := FramePhonemeWeight(p, frame)
	targetEnergy, targetSat := MulQ15(seg.TargetEnergyQ15, weight)
	if targetSat != nil {
		saturations = append(saturations, targetSat.Operation+":target_weight")
	}
	effectiveTargetDB := LinearToDBQ8(targetEnergy)
	desiredCorrectionDB := Q8(int64(effectiveTargetDB) - int64(loudnessDB))

	carry := newCalibrationCarry()
	if prev != nil {
		carry = *prev
	}

	result := Calibration{
		GainedEnergyQ15:       gained,
		EnergyQ15:             energy,
		Limited:               limited,
		LimitSource:           limitSource,
		LoudnessDBQ8:          loudnessDB,
		EffectiveTargetDBQ8:   effectiveTargetDB,
		DesiredCorrectionDBQ8: desiredCorrectionDB,
		State:                 carry.state,
		StableFrames:          carry.stableFrames,
		Saturations:           saturations,
	}

	if raw < ActiveRMSThreshold {
		carry.lastLoudnessDB = loudnessDB
		result.CorrectionDBQ8 = carry.smoothCorrection
		gainCorrection, convSat := DBQ8ToLinearQ15(carry.smoothCorrection)
		if convSat != nil {
			result.Saturations = append(result.Saturations, convSat.Operation+":correction_gain")
		}
		result.CorrectionGainQ15 = gainCorrection
		result.CalibratedDBQ8 = addClampedQ8(loudnessDB, carry.smoothCorrection)
		result.ResidualDBQ8 = Q8(int64(effectiveTargetDB) - int64(result.CalibratedDBQ8))
		result.State = carry.state
		result.StateReason = "silence_hold"
		return result, carry
	}

	smooth := floorAverageQ8(carry.smoothCorrection, desiredCorrectionDB)
	carry.smoothCorrection = smooth
	correctionGain, convSat := DBQ8ToLinearQ15(smooth)
	if convSat != nil {
		result.Saturations = append(result.Saturations, convSat.Operation+":correction_gain")
	}
	calibratedDB := addClampedQ8(loudnessDB, smooth)
	residual := Q8(int64(effectiveTargetDB) - int64(calibratedDB))

	reason := ""
	if !carry.hasActive {
		carry.state = StateUncalibrated
		carry.stableFrames = 0
		reason = "first_active_frame"
	} else {
		levelJump := absInt64(int64(loudnessDB) - int64(carry.lastLoudnessDB))
		inTolerance := absInt64(int64(residual)) <= int64(p.CalibrationTolDBQ8)
		switch {
		case levelJump >= int64(p.LevelJumpThresholdQ8):
			carry.state = StateRecalibrate
			carry.stableFrames = 0
			reason = "level_jump"
		case inTolerance:
			carry.stableFrames++
			if carry.state == StateCalibrated || carry.stableFrames >= p.SettleFrames {
				carry.state = StateCalibrated
				reason = "stable"
			} else {
				carry.state = StateRecalibrate
				reason = "settling"
			}
		default:
			carry.stableFrames = 0
			carry.state = StateRecalibrate
			reason = "residual_outside_tolerance"
		}
	}
	carry.hasActive = true
	carry.lastLoudnessDB = loudnessDB

	result.CorrectionDBQ8 = smooth
	result.CorrectionGainQ15 = correctionGain
	result.CalibratedDBQ8 = calibratedDB
	result.ResidualDBQ8 = residual
	result.State = carry.state
	result.StateReason = reason
	result.StableFrames = carry.stableFrames
	return result, carry
}

func computeFrameResult(p *Project, frame Frame, revision int64, provenance Provenance, prev *calibrationCarry) (FrameResult, calibrationCarry, error) {
	samples, err := SafeFrameSamples(p, frame)
	if err != nil {
		return FrameResult{}, calibrationCarry{}, err
	}
	features := ExtractFeatures(samples, p.SampleRateHz)
	seg := segmentByID(p, frame.SegmentID)
	if seg == nil {
		return FrameResult{}, calibrationCarry{}, &BusinessError{Code: CodeFrameBounds, SegmentID: frame.SegmentID, FrameIndex: frame.GlobalIndex, Detail: "segment not found"}
	}
	cal, carry := CalibrateFrame(p, seg, frame, features, prev)
	result := FrameResult{Frame: frame, Features: features, Calibration: cal, Provenance: provenance}
	if result.Provenance.Revision == 0 {
		result.Provenance.Revision = revision
	}
	return result, carry, nil
}
