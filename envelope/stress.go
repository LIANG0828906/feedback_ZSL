package envelope

import (
	"math"

	"dialoguefit/model"
)

// LocateStress returns one deterministic stress mark for every vowel.
// The score is dominated by normalized envelope energy; the phoneme F0
// contour supplies a small pitch-peak tie-break. Fine sample position comes
// from the earliest maximum absolute PCM sample in the selected frame.
func LocateStress(d model.Dialogue, frames []model.Frame) ([]model.StressMark, error) {
	marks := make([]model.StressMark, 0)
	for _, ph := range d.Phonemes {
		if ph.Category != model.CategoryVowel {
			continue
		}
		local := FramesForPhoneme(frames, ph)
		if len(local) == 0 {
			return nil, model.DecodeError(d.ID, "phonemes["+itoa(ph.Index)+"]", "no envelope frame center belongs to vowel")
		}
		minF0, maxF0 := f0Range(ph.F0)
		best := local[0]
		bestScore := -math.MaxFloat64
		for _, f := range local {
			f0 := f0AtCenter(ph, f.CenterSample)
			f0Norm := 0.0
			if maxF0 > minF0 {
				f0Norm = (f0 - minF0) / (maxF0 - minF0)
			}
			score := f.Value + 0.05*f0Norm
			if score > bestScore {
				bestScore = score
				best = f
			}
		}
		sample := peakPCMSample(d.PCM.Samples, best.StartSample, best.EndSample)
		pitchPeak := pitchPeakSample(ph)
		distance := absInt(sample - pitchPeak)
		marks = append(marks, model.StressMark{
			PhonemeID:       ph.ID,
			Index:           ph.Index,
			Sample:          sample,
			AlignedSample:   sample,
			Distance:        distance,
			WithinTolerance: distance <= d.Params.StressToleranceSamples*4,
		})
	}
	return marks, nil
}

func ApplyStretchAlignment(marks []model.StressMark, phonemes []model.Phoneme, segments []model.StretchSegment) []model.StressMark {
	out := append([]model.StressMark(nil), marks...)
	for i := range out {
		out[i].AlignedSample = WarpSample(out[i].Sample, phonemes, segments)
	}
	return out
}

func WarpSample(sample int, phonemes []model.Phoneme, segments []model.StretchSegment) int {
	for i, ph := range phonemes {
		if i >= len(segments) {
			break
		}
		if sample >= ph.StartSample && sample < ph.EndSample {
			offset := sample - ph.StartSample
			duration := ph.Duration()
			newDuration := segments[i].EndSample - segments[i].StartSample
			if duration <= 0 {
				return segments[i].StartSample
			}
			scaled := offset*newDuration*2 + duration
			return segments[i].StartSample + scaled/(2*duration)
		}
		if sample == ph.EndSample {
			return segments[i].EndSample
		}
	}
	return sample
}

func peakPCMSample(samples []int16, start, end int) int {
	start = maxInt(0, start)
	end = minInt(len(samples), end)
	if start >= end {
		return start
	}
	peak := start
	peakAmp := abs16(samples[start])
	for i := start + 1; i < end; i++ {
		a := abs16(samples[i])
		if a > peakAmp {
			peakAmp = a
			peak = i
		}
	}
	return peak
}

func pitchPeakSample(ph model.Phoneme) int {
	if len(ph.F0) == 0 {
		return ph.StartSample + ph.Duration()/2
	}
	bestIndex := 0
	best := ph.F0[0]
	for i := 1; i < len(ph.F0); i++ {
		if ph.F0[i] > best {
			best = ph.F0[i]
			bestIndex = i
		}
	}
	return ph.StartSample + bestIndex*ph.F0StepSamples
}

func f0Range(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	lo, hi := values[0], values[0]
	for _, v := range values[1:] {
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	return lo, hi
}

func f0AtCenter(ph model.Phoneme, center int) float64 {
	if len(ph.F0) == 0 || ph.F0StepSamples <= 0 {
		return 0
	}
	idx := (center - ph.StartSample + ph.F0StepSamples/2) / ph.F0StepSamples
	if idx < 0 {
		idx = 0
	}
	if idx >= len(ph.F0) {
		idx = len(ph.F0) - 1
	}
	return ph.F0[idx]
}

func abs16(v int16) int {
	if v < 0 {
		return -int(v)
	}
	return int(v)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
