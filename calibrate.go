package dialogue

// ClipCalibration is the solved gain adjustment and all calibrated frames.
type ClipCalibration struct {
	ClipID              string            `json:"clip_id"`
	GainAdjustBP        int64             `json:"gain_adjust_bp"`
	ReachableMaxLoudMDB int64             `json:"reachable_max_loud_mdb"`
	SumGainAdjustBP     int64             `json:"sum_gain_adjust_bp"`
	Frames              []CalibratedFrame `json:"frames"`
	Reachability        LoudnessGap       `json:"reachability"`
	Gap                 *LoudnessGap      `json:"gap,omitempty"`
}

func combineGainBP(sessionBP, clipBP int64) (int64, Truncation, error) {
	n, ok := checkedMulPositive(sessionBP, clipBP)
	if !ok {
		return 0, Truncation{}, overflowError("combined_gain_bp", sessionBP, clipBP)
	}
	v, tr := resolveRounding(n, BasisScale, RoundNearest, "combined_gain_bp",
		"session_bp*clip_bp/10000", 0)
	return v, tr, nil
}

func applyGainBP(rmsMicro, combinedBP int64, field string) (int64, Truncation, error) {
	n, ok := checkedMulPositive(rmsMicro, combinedBP)
	if !ok {
		return 0, Truncation{}, overflowError(field, rmsMicro, combinedBP)
	}
	v, tr := resolveRounding(n, BasisScale, RoundNearest, field, "rms_micro*gain_bp/10000", 0)
	return v, tr, nil
}

// ClipAggregateRMS computes whole-clip fixed-point RMS. It is frame-rate
// independent, so requantization cannot change the segment gain solution.
func ClipAggregateRMS(clip *Clip) (rmsMicro, energyFixed int64, tr Truncation, pErr *Error) {
	n := int64(len(clip.Samples))
	if n == 0 {
		return 0, 0, Truncation{}, &Error{Code: CodeDecode, Message: "clip has no PCM samples", ClipID: clip.ID}
	}
	var sumSquares int64
	for _, sample := range clip.Samples {
		v := int64(sample)
		sumSquares += v * v
	}
	scaled, ok := checkedMulPositive(sumSquares, EnergyScale)
	if !ok {
		return 0, 0, Truncation{}, overflowError("clip_energy_fixed", sumSquares, EnergyScale)
	}
	den := n * 32768 * 32768
	energy, energyTrunc := resolveRounding(scaled, den, RoundNearest,
		"clip_energy_fixed", "sum(sample^2)*1e6/(N*32768^2)", 0)
	return IntSqrt(energy * LinearScale), energy, energyTrunc, nil
}

// CalibrateClip solves one clip-level gain adjustment from whole-clip energy,
// then derives every calibrated frame loudness.
func CalibrateClip(project *Project, clip *Clip, features []FrameFeature) (*ClipCalibration, *LoudnessGap, error) {
	if len(features) == 0 {
		return nil, nil, &Error{Code: CodeStage, Message: "clip has no frames to calibrate", ClipID: clip.ID}
	}
	rawRMS, _, avgTrunc, aggErr := ClipAggregateRMS(clip)
	if aggErr != nil {
		return nil, nil, aggErr
	}
	if rawRMS < SilenceRMSMicro {
		// A fully silent clip has no reachable loudness. Report a gap rather
		// than inventing a huge gain.
		maxLoud, _, err := LinearToDBMilli(0, "reachable_max_loud_mdb")
		if err != nil {
			return nil, nil, err
		}
		gap := &LoudnessGap{
			ClipID:              clip.ID,
			Reachable:           false,
			CurrentLoudMDB:      MinLoudDBMilli,
			ReachableMaxLoudMDB: maxLoud,
			TargetLoudMDB:       project.TargetLoudMDB,
			GapMDB:              project.TargetLoudMDB - maxLoud,
			RequiredAdjustBP:    0,
			MaxAllowedAdjustBP:  project.MaxAdjustBP,
		}
		return &ClipCalibration{
			ClipID:       clip.ID,
			Frames:       make([]CalibratedFrame, 0, len(features)),
			Reachability: *gap,
			Gap:          gap,
		}, gap, nil
	}

	baseCombined, combineTrunc, err := combineGainBP(project.SessionGainBP, clip.GainBP)
	if err != nil {
		return nil, nil, err
	}
	currentRMS, gainTrunc, err := applyGainBP(rawRMS, baseCombined, "current_rms_micro")
	if err != nil {
		return nil, nil, err
	}
	currentLoud, _, err := LinearToDBMilli(currentRMS, "loud_before_mdb")
	if err != nil {
		return nil, nil, err
	}

	requiredTotalNumerator, ok := checkedMulPositive(project.TargetAmpMicro, BasisScale)
	if !ok {
		return nil, nil, overflowError("required_total_gain_bp", project.TargetAmpMicro, BasisScale)
	}
	requiredTotalBP, _ := resolveRounding(requiredTotalNumerator, rawRMS, RoundCeil,
		"required_total_gain_bp", "target_amp*10000/raw_rms", 0)
	requiredClipNumerator, ok := checkedMulPositive(requiredTotalBP, BasisScale)
	if !ok {
		return nil, nil, overflowError("required_clip_gain_bp", requiredTotalBP, BasisScale)
	}
	requiredClipBP, _ := resolveRounding(requiredClipNumerator, project.SessionGainBP, RoundCeil,
		"required_clip_gain_bp", "required_total*10000/session_gain", 0)
	idealAdjust := requiredClipBP - clip.GainBP

	actualAdjust := idealAdjust
	if actualAdjust > project.MaxAdjustBP {
		actualAdjust = project.MaxAdjustBP
	}
	if actualAdjust < -project.MaxAdjustBP {
		actualAdjust = -project.MaxAdjustBP
	}

	maxClipGain := clip.GainBP + project.MaxAdjustBP
	maxCombined, _, err := combineGainBP(project.SessionGainBP, maxClipGain)
	if err != nil {
		return nil, nil, err
	}
	maxRMS, _, err := applyGainBP(rawRMS, maxCombined, "reachable_max_rms_micro")
	if err != nil {
		return nil, nil, err
	}
	maxLoud, _, err := LinearToDBMilli(maxRMS, "reachable_max_loud_mdb")
	if err != nil {
		return nil, nil, err
	}
	gapMDB := project.TargetLoudMDB - maxLoud
	reachable := maxRMS >= project.TargetAmpMicro || gapMDB <= LoudnessToleranceMDB
	gap := &LoudnessGap{
		ClipID:              clip.ID,
		Reachable:           reachable,
		CurrentLoudMDB:      currentLoud,
		ReachableMaxLoudMDB: maxLoud,
		TargetLoudMDB:       project.TargetLoudMDB,
		GapMDB:              max64(gapMDB, 0),
		RequiredAdjustBP:    idealAdjust,
		MaxAllowedAdjustBP:  project.MaxAdjustBP,
	}

	adjustedClipGain := clip.GainBP + actualAdjust
	adjustedCombined, adjustedCombineTrunc, err := combineGainBP(project.SessionGainBP, adjustedClipGain)
	if err != nil {
		return nil, nil, err
	}
	frames := make([]CalibratedFrame, 0, len(features))
	for _, f := range features {
		beforeRMS, beforeTrunc, err := applyGainBP(f.RMSMicro, baseCombined, "frame_before_rms_micro")
		if err != nil {
			return nil, nil, err
		}
		beforeLoud, _, err := LinearToDBMilli(beforeRMS, "loud_before_mdb")
		if err != nil {
			return nil, nil, err
		}
		afterRMS, afterTrunc, err := applyGainBP(f.RMSMicro, adjustedCombined, "frame_after_rms_micro")
		if err != nil {
			return nil, nil, err
		}
		afterLoud, _, err := LinearToDBMilli(afterRMS, "loud_after_mdb")
		if err != nil {
			return nil, nil, err
		}
		silent := f.RMSMicro == 0
		if silent {
			beforeLoud = MinLoudDBMilli
			afterLoud = MinLoudDBMilli
		}
		truncs := []Truncation{avgTrunc, combineTrunc, gainTrunc, beforeTrunc, afterTrunc, adjustedCombineTrunc}
		frames = append(frames, CalibratedFrame{
			ClipID:         clip.ID,
			FrameNo:        f.FrameNo,
			RangeID:        f.RangeID,
			RawRMSMicro:    f.RMSMicro,
			LoudBeforeMDB:  beforeLoud,
			LoudAfterMDB:   afterLoud,
			TargetLoudMDB:  project.TargetLoudMDB,
			GainAdjustBP:   actualAdjust,
			CombinedGainBP: adjustedCombined,
			Silent:         silent,
			Truncations:    truncs,
		})
	}
	cal := &ClipCalibration{
		ClipID:              clip.ID,
		GainAdjustBP:        actualAdjust,
		ReachableMaxLoudMDB: maxLoud,
		SumGainAdjustBP:     actualAdjust,
		Frames:              frames,
		Reachability:        *gap,
	}
	if !reachable {
		cal.Gap = gap
		return cal, gap, nil
	}
	return cal, nil, nil
}

// LoudnessToleranceMDB allows integer dB/amplitude round-trip quantization.
const LoudnessToleranceMDB int64 = 100

// CalibrateProject calibrates every clip and aggregates reachability gaps.
func CalibrateProject(project *Project, features map[string][]FrameFeature) (map[string]*ClipCalibration, *Error) {
	out := make(map[string]*ClipCalibration, len(project.Clips))
	gaps := make([]LoudnessGap, 0, len(project.Clips))
	unreachable := false
	absoluteBudget := int64(0)
	for _, clip := range project.Clips {
		fs := features[clip.ID]
		cal, gap, err := CalibrateClip(project, clip, fs)
		if err != nil {
			if be, ok := err.(*Error); ok {
				return nil, be
			}
			return nil, &Error{Code: CodeStage, Message: err.Error(), ClipID: clip.ID}
		}
		out[clip.ID] = cal
		if gap != nil {
			unreachable = true
		}
		if cal != nil {
			gaps = append(gaps, cal.Reachability)
		}
		if cal != nil {
			absoluteBudget += abs64(cal.GainAdjustBP)
		}
	}
	if unreachable {
		return out, &Error{
			Code:    CodeLoud,
			Message: "target loudness is unreachable within declared gain upper bounds",
			Details: LoudnessErrorDetails{Gaps: gaps, TotalBudgetBP: project.TotalBudgetBP},
		}
	}
	if project.TotalBudgetBP > 0 && absoluteBudget > project.TotalBudgetBP {
		return out, &Error{
			Code:     CodePara,
			Message:  "sum of absolute gain adjustments exceeds total budget",
			Field:    "total_budget_bp",
			Operands: []int64{absoluteBudget, project.TotalBudgetBP},
			Details:  LoudnessErrorDetails{Gaps: gaps, TotalBudgetBP: project.TotalBudgetBP},
		}
	}
	return out, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
