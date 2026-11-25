package dialoguecal

import (
	"math"
)

type TruthFrame struct {
	GlobalIndex    int              `json:"global_index"`
	SegmentID      string           `json:"segment_id"`
	IndexInSegment int              `json:"index_in_segment"`
	RMSQ15         Q15              `json:"rms_q15"`
	ZCRCount       int              `json:"zcr_count"`
	ZCRQ15         Q15              `json:"zcr_q15"`
	F0HzQ8         Q8               `json:"f0_hz_q8"`
	Voiced         bool             `json:"voiced"`
	EnergyQ15      Q15              `json:"energy_q15"`
	Limited        bool             `json:"limited"`
	LoudnessDBQ8   Q8               `json:"loudness_db_q8"`
	CorrectionDBQ8 Q8               `json:"correction_db_q8"`
	CalibratedDBQ8 Q8               `json:"calibrated_db_q8"`
	State          CalibrationState `json:"state"`
	CriticalID     string           `json:"critical_id,omitempty"`
}

type TruthSet struct {
	Tolerances Tolerances   `json:"tolerances"`
	Frames     []TruthFrame `json:"frames"`
}

type referenceCarry struct {
	smoothCorrection float64
	state            CalibrationState
	stableFrames     int
	lastLoudnessDB   float64
	hasActive        bool
}

func GenerateTruth(p *Project, frames []Frame) *TruthSet {
	truth := &TruthSet{Tolerances: DefaultTolerances(), Frames: make([]TruthFrame, 0, len(frames))}
	carries := map[string]*referenceCarry{}
	for _, frame := range frames {
		seg := segmentByID(p, frame.SegmentID)
		if seg == nil {
			continue
		}
		carry := carries[seg.ID]
		if carry == nil {
			carry = &referenceCarry{state: StateUncalibrated}
			carries[seg.ID] = carry
		}
		tf := referenceFrame(p, seg, frame, carry)
		truth.Frames = append(truth.Frames, tf)
	}
	return truth
}

func frameCenterPhoneme(p *Project, f Frame) *Phoneme {
	center := (f.StartSample + f.EndSample) / 2
	for i := range p.Phonemes {
		ph := &p.Phonemes[i]
		if center >= ph.StartSample && center < ph.EndSample {
			return ph
		}
	}
	for _, id := range f.PhonemeIDs {
		return phonemeByID(p, id)
	}
	return nil
}

func referenceFrame(p *Project, seg *Segment, f Frame, carry *referenceCarry) TruthFrame {
	samples := p.PCM[f.StartSample:f.EndSample]
	n := float64(len(samples))
	var sumSquares float64
	zcr := 0
	for i, s := range samples {
		sumSquares += float64(s) * float64(s)
		if i > 0 {
			prev := samples[i-1]
			if (prev < 0 && s >= 0) || (prev >= 0 && s < 0) {
				zcr++
			}
		}
	}
	rms := math.Sqrt(sumSquares / n)
	rmsQ15 := Q15(math.Round(rms))
	zcrQ15 := Q15((int64(zcr) * Q15Scale) / int64(len(samples)-1))

	ph := frameCenterPhoneme(p, f)
	var f0 Q8
	voiced := false
	weight := 1.0
	if ph != nil {
		weight = float64(ph.WeightPermille) / 1000.0
		if ph.Class == ClassVowel {
			progress := 0.5
			if ph.EndSample > ph.StartSample {
				center := (f.StartSample + f.EndSample) / 2
				progress = float64(center-ph.StartSample) / float64(ph.EndSample-ph.StartSample)
				progress = math.Max(0, math.Min(1, progress))
			}
			hz := float64(ph.F0StartHzQ8)/256.0 + (float64(ph.F0EndHzQ8-ph.F0StartHzQ8)/256.0)*progress
			f0 = Q8(math.Round(hz * 256))
			voiced = true
		}
	}

	gain := float64(seg.GainPermille) / 1000.0
	cap := float64(p.EnergyCapPermille) / 1000.0 * float64(Q15Scale)
	gained := rms * gain
	energy := math.Min(gained, cap)
	limited := gained > cap
	db := -96.0
	if energy > 0 {
		db = 20 * math.Log10(energy/float64(Q15Scale))
	}
	targetEnergy := float64(seg.TargetLevelPermille) / 1000.0 * float64(Q15Scale) * weight
	targetDB := 20 * math.Log10(math.Max(targetEnergy, 1)/float64(Q15Scale))
	desired := targetDB - db

	state := carry.state
	stable := carry.stableFrames
	smooth := carry.smoothCorrection
	if rms < float64(ActiveRMSThreshold) {
		state = carry.state
		carry.lastLoudnessDB = db
	} else {
		smooth = (carry.smoothCorrection + desired) / 2.0
		correctionQ8 := Q8(math.Round(smooth * 256))
		calibratedDB := db + smooth
		residual := targetDB - calibratedDB
		if !carry.hasActive {
			state = StateUncalibrated
			stable = 0
		} else {
			jump := math.Abs(db - carry.lastLoudnessDB)
			inTol := math.Abs(residual) <= float64(p.CalibrationTolDBQ8)/256.0
			switch {
			case jump >= float64(p.LevelJumpThresholdQ8)/256.0:
				state = StateRecalibrate
				stable = 0
			case inTol:
				stable++
				if carry.state == StateCalibrated || stable >= p.SettleFrames {
					state = StateCalibrated
				} else {
					state = StateRecalibrate
				}
			default:
				stable = 0
				state = StateRecalibrate
			}
		}
		carry.hasActive = true
		carry.lastLoudnessDB = db
		carry.smoothCorrection = smooth
		carry.stableFrames = stable
		carry.state = state
		_ = correctionQ8
	}

	return TruthFrame{
		GlobalIndex: f.GlobalIndex, SegmentID: f.SegmentID, IndexInSegment: f.IndexInSegment,
		RMSQ15: rmsQ15, ZCRCount: zcr, ZCRQ15: zcrQ15, F0HzQ8: f0, Voiced: voiced,
		EnergyQ15: Q15(math.Round(energy)), Limited: limited,
		LoudnessDBQ8: Q8(math.Round(db * 256)), CorrectionDBQ8: Q8(math.Round(smooth * 256)),
		CalibratedDBQ8: Q8(math.Round((db + smooth) * 256)), State: state,
	}
}

func CompareResults(results []FrameResult, truth *TruthSet) []FrameComparison {
	if truth == nil {
		return nil
	}
	byIndex := map[int]TruthFrame{}
	for _, tf := range truth.Frames {
		byIndex[tf.GlobalIndex] = tf
	}
	comparisons := make([]FrameComparison, 0, len(results))
	for _, result := range results {
		tf, ok := byIndex[result.Frame.GlobalIndex]
		if !ok {
			continue
		}
		cmp := FrameComparison{GlobalIndex: result.Frame.GlobalIndex}
		cmp.EnergyDeltaQ15 = absInt64(int64(result.Features.RMSQ15) - int64(tf.RMSQ15))
		cmp.EnergyWithin = cmp.EnergyDeltaQ15 <= int64(truth.Tolerances.EnergyQ15)
		cmp.ZCRDeltaQ15 = absInt64(int64(result.Features.ZCRQ15) - int64(tf.ZCRQ15))
		cmp.ZCRWithin = result.Features.ZCRCount == tf.ZCRCount && cmp.ZCRDeltaQ15 <= int64(truth.Tolerances.ZCRQ15)

		if tf.F0HzQ8 > 0 {
			best := int64(1 << 62)
			for _, cand := range result.Features.F0Candidates {
				delta := absInt64(int64(cand.HzQ8) - int64(tf.F0HzQ8))
				if delta < best {
					best = delta
				}
			}
			if best == int64(1<<62) {
				best = 1 << 62
			}
			cmp.F0DeltaHzQ8 = best
			cmp.F0Within = best <= int64(truth.Tolerances.F0HzQ8)
		} else {
			cmp.F0DeltaHzQ8 = 0
			cmp.F0Within = !result.Features.Voiced
			if result.Features.Voiced {
				cmp.F0DeltaHzQ8 = int64(truth.Tolerances.F0HzQ8) + 1
			}
		}

		cmp.LoudnessDeltaDBQ8 = absInt64(int64(result.Calibration.LoudnessDBQ8) - int64(tf.LoudnessDBQ8))
		cmp.LoudnessWithin = cmp.LoudnessDeltaDBQ8 <= int64(truth.Tolerances.LoudnessDBQ8)
		cmp.CorrectionDeltaDBQ8 = absInt64(int64(result.Calibration.CorrectionDBQ8) - int64(tf.CorrectionDBQ8))
		cmp.CorrectionWithin = cmp.CorrectionDeltaDBQ8 <= int64(truth.Tolerances.CorrectionDBQ8)
		cmp.StateMatch = result.Calibration.State == tf.State
		cmp.AllWithin = cmp.EnergyWithin && cmp.ZCRWithin && cmp.F0Within && cmp.LoudnessWithin && cmp.CorrectionWithin && (!truth.Tolerances.StateMustMatch || cmp.StateMatch)
		if !cmp.AllWithin {
			cmp.Note = "outside declared tolerance"
		}
		comparisons = append(comparisons, cmp)
	}
	return comparisons
}
