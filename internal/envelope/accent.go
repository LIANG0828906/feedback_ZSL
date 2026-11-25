package envelope

import (
	"fmt"

	"prosodyalign/internal/errs"
	"prosodyalign/internal/model"
)

func PCMPeakSample(pcm []int16, start, length int64) int64 {
	lo := int(start)
	hi := int(start + length)
	if lo < 0 {
		lo = 0
	}
	if hi > len(pcm) {
		hi = len(pcm)
	}
	peak := lo
	var peakAmp int64 = -1
	for i := lo; i < hi; i++ {
		a := absInt16(pcm[i])
		if a > peakAmp {
			peakAmp = a
			peak = i
		}
	}
	return int64(peak)
}

func LocateAccent(p *model.Project, frames []model.FrameResult, tolerance int64) (*model.AccentResult, error) {
	if len(frames) == 0 {
		return nil, errs.Env(0, 0, "cannot locate accent without envelope frames")
	}
	best := frames[0]
	foundVowel := false
	for _, f := range frames {
		isVowel := f.PhonemeIndex >= 0 && f.PhonemeIndex < len(p.Phonemes) && p.Phonemes[f.PhonemeIndex].Kind == model.Vowel
		if !isVowel && foundVowel {
			continue
		}
		if isVowel && !foundVowel {
			foundVowel = true
			best = f
			continue
		}
		if foundVowel && !isVowel {
			continue
		}
		if f.SmoothedLoudness > best.SmoothedLoudness {
			best = f
		}
	}

	length := int64(p.EnvelopeParams.FrameLengthSamples)
	loudnessPeak := PCMPeakSample(p.PCM, best.StartSample, length)
	phIndex := best.PhonemeIndex
	if phIndex < 0 || phIndex >= len(p.Phonemes) {
		phIndex = phonemeAt(p, loudnessPeak)
	}
	ph := p.Phonemes[phIndex]

	pitchPeak := ph.StartSample + ph.DurationSamples()/2
	var bestF0 int64 = -1
	for _, point := range ph.F0 {
		if point.MilliHz > bestF0 {
			bestF0 = point.MilliHz
			pitchPeak = ph.StartSample + point.OffsetSample
		}
	}
	distance := loudnessPeak - pitchPeak
	if distance < 0 {
		distance = -distance
	}
	result := &model.AccentResult{
		Sample: loudnessPeak, PhonemeIndex: phIndex, LoudnessPeak: loudnessPeak,
		PitchPeak: pitchPeak, DistanceSamples: distance, ToleranceSamples: tolerance,
		FinalFrameIndex: best.FrameIndex,
	}
	if distance > tolerance {
		return result, errs.Fit(phIndex, fmt.Sprintf("loudness peak and pitch peak differ by %d samples > tolerance %d", distance, tolerance), map[string]any{
			"loudness_peak_sample": loudnessPeak,
			"pitch_peak_sample":    pitchPeak,
			"measured_distance":    distance,
			"tolerance_samples":    tolerance,
			"left_measured":        loudnessPeak,
			"right_measured":       pitchPeak,
		})
	}
	return result, nil
}
