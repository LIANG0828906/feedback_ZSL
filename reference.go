package dialogue

// BuildReferenceTruth constructs generator-owned expected values by walking
// the analytically generated PCM directly. It intentionally duplicates the
// declared integer rules in a straight-line reference form rather than
// calling the cached pipeline.
func BuildReferenceTruth(p *Project) (*TruthManifest, error) {
	manifest := &TruthManifest{
		Seed:           p.Seed,
		DialogueID:     p.DialogueID,
		SampleRate:     p.SampleRate,
		FrameRate:      p.FrameRate,
		TargetAmpMicro: p.TargetAmpMicro,
		TargetLoudMDB:  p.TargetLoudMDB,
		Clips:          make([]*ClipTruth, 0, len(p.Clips)),
	}
	frameLength := p.SampleRate / p.FrameRate
	for _, clip := range p.Clips {
		intervals := QuantizeClip(p, clip)
		features := make([]FrameFeature, 0, len(intervals))
		for _, interval := range intervals {
			count := int(interval.FrameEndSample - interval.FrameStartSample)
			var sumSquares int64
			crossings := 0
			peakAbs := 0
			peakOffset := -1
			var previous int16
			havePrevious := false
			for global := interval.FrameStartSample; global < interval.FrameEndSample; global++ {
				idx := global - clip.StartSample
				var sample int16
				if idx >= 0 && idx < int64(len(clip.Samples)) {
					sample = clip.Samples[idx]
				}
				v := int64(sample)
				sumSquares += v * v
				if havePrevious && ((previous >= 0 && sample < 0) || (previous < 0 && sample >= 0)) {
					crossings++
				}
				previous = sample
				havePrevious = true
				absSample := int(sample)
				if absSample < 0 {
					absSample = -absSample
				}
				if absSample > peakAbs {
					peakAbs = absSample
					peakOffset = int(global - interval.FrameStartSample)
				}
			}
			scaled, _ := checkedMulPositive(sumSquares, EnergyScale)
			energyDen := int64(count) * 32768 * 32768
			energy, energyTrunc := resolveRounding(scaled, energyDen, RoundFloor,
				"energy_fixed", "reference_sum", interval.FrameStartSample)
			zcrNumerator, _ := checkedMulPositive(int64(crossings), BasisScale)
			zcr, zcrTrunc := resolveRounding(zcrNumerator, int64(count-1), RoundFloor,
				"zcr_bp", "reference_crossings", interval.FrameStartSample)
			feature := FrameFeature{
				ClipID:           clip.ID,
				FrameNo:          interval.FrameNo,
				RangeID:          interval.RangeID,
				FrameStartSample: interval.FrameStartSample,
				FrameEndSample:   interval.FrameEndSample,
				EvaluatedSamples: count,
				EnergyFixed:      energy,
				RMSMicro:         IntSqrt(energy * LinearScale),
				ZCRBP:            zcr,
				PeakSample:       -1,
				PeakOffset:       -1,
				Truncations:      []Truncation{energyTrunc, zcrTrunc},
			}
			if peakAbs > 0 && peakOffset >= 0 {
				feature.PeakOffset = peakOffset
				feature.PeakAmp = peakAbs
				feature.PeakSample = interval.FrameStartSample + int64(peakOffset)
				feature.Transient = peakAbs >= TransientThreshold
			}
			features = append(features, feature)
		}

		rawRMS, _, _, _ := ClipAggregateRMS(clip)
		baseCombined, _, _ := combineGainBP(p.SessionGainBP, clip.GainBP)
		reqTotalNum, _ := checkedMulPositive(p.TargetAmpMicro, BasisScale)
		reqTotal, _ := resolveRounding(reqTotalNum, rawRMS, RoundCeil, "required_total", "ref", 0)
		reqClipNum, _ := checkedMulPositive(reqTotal, BasisScale)
		reqClip, _ := resolveRounding(reqClipNum, p.SessionGainBP, RoundCeil, "required_clip", "ref", 0)
		adjust := reqClip - clip.GainBP
		if adjust > p.MaxAdjustBP {
			adjust = p.MaxAdjustBP
		}
		if adjust < -p.MaxAdjustBP {
			adjust = -p.MaxAdjustBP
		}
		maxCombined, _, _ := combineGainBP(p.SessionGainBP, clip.GainBP+p.MaxAdjustBP)
		maxRMS, _, _ := applyGainBP(rawRMS, maxCombined, "max_rms")
		maxLoud, _, _ := LinearToDBMilli(maxRMS, "max_loud")
		adjustedCombined, _, _ := combineGainBP(p.SessionGainBP, clip.GainBP+adjust)
		calibrations := make([]CalibratedFrame, 0, len(features))
		for _, feature := range features {
			beforeRMS, _, _ := applyGainBP(feature.RMSMicro, baseCombined, "before_rms")
			afterRMS, _, _ := applyGainBP(feature.RMSMicro, adjustedCombined, "after_rms")
			beforeLoud, _, _ := LinearToDBMilli(beforeRMS, "before_loud")
			afterLoud, _, _ := LinearToDBMilli(afterRMS, "after_loud")
			silent := feature.RMSMicro == 0
			if silent {
				beforeLoud = MinLoudDBMilli
				afterLoud = MinLoudDBMilli
			}
			calibrations = append(calibrations, CalibratedFrame{
				ClipID:         clip.ID,
				FrameNo:        feature.FrameNo,
				RangeID:        feature.RangeID,
				RawRMSMicro:    feature.RMSMicro,
				LoudBeforeMDB:  beforeLoud,
				LoudAfterMDB:   afterLoud,
				TargetLoudMDB:  p.TargetLoudMDB,
				GainAdjustBP:   adjust,
				CombinedGainBP: adjustedCombined,
				Silent:         silent,
			})
		}

		clipTruth := &ClipTruth{
			ClipID:              clip.ID,
			AudioHash:           clip.AudioHash,
			TransientSample:     clip.TransientSample,
			TransientAmp:        26_000,
			GainAdjustBP:        adjust,
			ReachableMaxLoudMDB: maxLoud,
			SampleChecks:        buildSampleChecks(p, clip, frameLength),
			Features:            features,
			Calibration:         calibrations,
		}
		manifest.Clips = append(manifest.Clips, clipTruth)
		clip.Truth = clipTruth
	}
	return manifest, nil
}
