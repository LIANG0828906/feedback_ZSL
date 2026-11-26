package loudness

import (
	"crypto/sha256"
	"fmt"
	"sort"
)

// CalibrationReport contains all calibrated clip results.
type CalibrationReport struct {
	TargetLoudnessDBQ8 int64              `json:"targetLoudnessDbQ8"`
	ReachableUpperDBQ8 int64              `json:"reachableUpperDbQ8"`
	Clips              map[int]ClipResult `json:"clips"`
	Truncations        []Truncation       `json:"truncations,omitempty"`
}

// CalibrateProject applies session gain, solves each clip's required gain
// adjustment, clamps at MaxGainQ12, and verifies per-clip target loudness.
func CalibrateProject(project *Project, measurements map[int]ClipMeasurement) (*CalibrationReport, error) {
	if err := validateProjectBasics(project); err != nil {
		return nil, err
	}
	report := &CalibrationReport{
		TargetLoudnessDBQ8: project.TargetLoudnessDBQ8,
		ReachableUpperDBQ8: int64(^uint64(0) >> 1),
		Clips:              make(map[int]ClipResult, len(measurements)),
	}
	limited := make([]int, 0)
	for _, clip := range project.Clips {
		measurement, ok := measurements[clip.ID]
		if !ok {
			return nil, &DomainError{Code: ESchema, Path: fmt.Sprintf("$.clips[%d]", indexOfClip(project, clip.ID)), Detail: "measurement is missing"}
		}
		result, maxLoudness, err := calibrateClip(project, clip, measurement)
		if err != nil {
			return nil, err
		}
		report.Clips[clip.ID] = result
		if maxLoudness < report.ReachableUpperDBQ8 {
			report.ReachableUpperDBQ8 = maxLoudness
		}
		if result.GainLimited {
			limited = append(limited, clip.ID)
		}
	}
	sort.Ints(limited)
	if report.ReachableUpperDBQ8 < project.TargetLoudnessDBQ8-project.LoudnessToleranceQ8 {
		return report, &DomainError{
			Code: ELoud, Path: "$.targetLoudnessDBQ8",
			Target: project.TargetLoudnessDBQ8, Reachable: report.ReachableUpperDBQ8,
			LimitedClips: limited,
			Detail: fmt.Sprintf("target q8=%d is above reachable upper q8=%d",
				project.TargetLoudnessDBQ8, report.ReachableUpperDBQ8),
		}
	}
	for id, result := range report.Clips {
		if result.GainLimited && result.CalibratedSegmentDBQ8 < project.TargetLoudnessDBQ8-project.LoudnessToleranceQ8 {
			return report, &DomainError{
				Code: ELoud, Path: fmt.Sprintf("$.clips[%d].gain", indexOfClip(project, id)),
				Target: project.TargetLoudnessDBQ8, Reachable: maxInt64(report.ReachableUpperDBQ8, result.CalibratedSegmentDBQ8),
				LimitedClips: []int{id},
				Detail:       "clip gain bound prevents reaching target",
			}
		}
	}
	return report, nil
}

func calibrateClip(project *Project, clip Clip, measurement ClipMeasurement) (ClipResult, int64, error) {
	path := fmt.Sprintf("$.clips[%d].calibration", indexOfClip(project, clip.ID))
	currentRms, sessionTrace, err := MulDivAt(path+".sessionGain", measurement.SegmentRmsQ16, project.SessionGainQ12, InternalGainScale, RoundHalfUp)
	if err != nil {
		return ClipResult{}, 0, err
	}
	if currentRms <= 0 {
		return ClipResult{}, 0, &DomainError{Code: ELoud, Path: path, Target: project.TargetLoudnessDBQ8, Reachable: 0, LimitedClips: []int{clip.ID}, Detail: "measured clip is silent"}
	}
	currentDB, currentTrace, err := LinearToDBQ16(currentRms, path+".currentDB")
	if err != nil {
		return ClipResult{}, 0, err
	}
	desiredDelta, err := CheckedSub(project.TargetLoudnessDBQ8, currentDB, path+".desiredDelta")
	if err != nil {
		return ClipResult{}, 0, err
	}
	desiredGain, desiredTrace, err := DBQ8ToGainQ12(desiredDelta, path+".desiredGain")
	if err != nil {
		return ClipResult{}, 0, err
	}
	if desiredGain < 1 {
		desiredGain = 1
	}
	finalGain := desiredGain
	gainLimited := false
	if finalGain > project.MaxGainQ12 {
		finalGain = project.MaxGainQ12
		gainLimited = true
	}
	calibratedRms, finalGainTrace, err := MulDivAt(path+".gainAdjustment", currentRms, finalGain, InternalGainScale, RoundHalfUp)
	if err != nil {
		return ClipResult{}, 0, err
	}
	maxRms, maxGainTrace, err := MulDivAt(path+".maxGain", currentRms, project.MaxGainQ12, InternalGainScale, RoundHalfUp)
	if err != nil {
		return ClipResult{}, 0, err
	}
	maxLoudness, maxDBTrace, err := LinearToDBQ16(maxRms, path+".maxDB")
	if err != nil {
		return ClipResult{}, 0, err
	}
	calibratedDB, calibratedTrace, err := LinearToDBQ16(calibratedRms, path+".calibratedDB")
	if err != nil {
		return ClipResult{}, 0, err
	}
	result := ClipResult{
		ClipID: clip.ID, ContentHash: measurement.ContentHash, PCMHash: measurement.PCMHash,
		FrameLength: measurement.FrameLength, FrameShift: measurement.FrameShift,
		GainAdjustmentQ12: finalGain, DesiredGainQ12: desiredGain, GainLimited: gainLimited,
		MeasuredSegmentEnergyQ32: measurement.SegmentEnergyQ32,
		MeasuredSegmentRmsQ16:    measurement.SegmentRmsQ16,
		MeasuredSegmentDBQ8:      measurement.SegmentDBQ8,
		CalibratedSegmentDBQ8:    calibratedDB, CalibratedSegmentRmsQ16: calibratedRms,
		MeasurementTruncations: append([]Truncation(nil), measurement.Truncations...),
		Truncations:            []Truncation{sessionTrace, currentTrace, desiredTrace, finalGainTrace, maxGainTrace, maxDBTrace, calibratedTrace},
	}
	energy, err := CheckedMul(calibratedRms, calibratedRms, path+".calibratedEnergy")
	if err != nil {
		return ClipResult{}, 0, err
	}
	result.CalibratedSegmentEnergyQ32 = energy
	result.Frames = make([]FrameEnvelope, len(measurement.Frames))
	for i, frame := range measurement.Frames {
		frameCopy := frame
		frameCopy.Truncations = append([]Truncation(nil), frame.Truncations...)
		withSession, frameSessionTrace, err := MulDivAt(fmt.Sprintf("%s.frames[%d].sessionGain", path, i), frame.RmsQ16, project.SessionGainQ12, InternalGainScale, RoundHalfUp)
		if err != nil {
			return ClipResult{}, 0, err
		}
		calibratedFrameRms, frameGainTrace, err := MulDivAt(fmt.Sprintf("%s.frames[%d].gainAdjustment", path, i), withSession, finalGain, InternalGainScale, RoundHalfUp)
		if err != nil {
			return ClipResult{}, 0, err
		}
		frameEnergy, err := CheckedMul(calibratedFrameRms, calibratedFrameRms, fmt.Sprintf("%s.frames[%d].calibratedEnergy", path, i))
		if err != nil {
			return ClipResult{}, 0, err
		}
		frameDB, frameDBTrace, err := LinearToDBQ16(calibratedFrameRms, fmt.Sprintf("%s.frames[%d].calibratedDB", path, i))
		if err != nil {
			return ClipResult{}, 0, err
		}
		frameCopy.CalibratedEnergyQ32 = frameEnergy
		frameCopy.CalibratedRmsQ16 = calibratedFrameRms
		frameCopy.CalibratedDBQ8 = frameDB
		frameCopy.Truncations = append(frameCopy.Truncations, frameSessionTrace, frameGainTrace, frameDBTrace)
		result.Frames[i] = frameCopy
	}
	result.EnvelopeDigest = digestFrames(result.Frames)
	return result, maxLoudness, nil
}

func digestFrames(frames []FrameEnvelope) string {
	hasher := sha256.New()
	for _, frame := range frames {
		writeField(hasher, "range", frame.RangeID)
		writeField(hasher, "energy", fmt.Sprintf("%d", frame.EnergyQ32))
		writeField(hasher, "rms", fmt.Sprintf("%d", frame.RmsQ16))
		writeField(hasher, "measuredDB", fmt.Sprintf("%d", frame.MeasuredDBQ8))
		writeField(hasher, "calibratedEnergy", fmt.Sprintf("%d", frame.CalibratedEnergyQ32))
		writeField(hasher, "calibratedDB", fmt.Sprintf("%d", frame.CalibratedDBQ8))
	}
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

// MeasureAndCalibrate is the one-shot integration path.
func MeasureAndCalibrate(project *Project) (*CalibrationReport, error) {
	measurements, err := MeasureProject(project)
	if err != nil {
		return nil, err
	}
	return CalibrateProject(project, measurements)
}
