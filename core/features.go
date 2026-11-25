package dialoguecal

import (
	"math/bits"
	"sort"
)

const VoicingThresholdQ15 Q15 = 11000

func isqrt64(value uint64) uint64 {
	if value == 0 {
		return 0
	}
	root := uint64(1) << ((uint(bits.Len64(value)) + 1) / 2)
	for {
		next := (root + value/root) / 2
		if next >= root {
			return root
		}
		root = next
	}
}

// ExtractFeatures computes frame features using integer sample arithmetic.
// The caller supplies an already bounds-checked slice.
func ExtractFeatures(samples []int16, sampleRate int) Features {
	n := len(samples)
	if n == 0 {
		return Features{}
	}
	var sumSquares int64
	zeroCrossings := 0
	for i, sample := range samples {
		v := int64(sample)
		sumSquares += v * v
		if i > 0 {
			prev := int64(samples[i-1])
			if (prev < 0 && v >= 0) || (prev >= 0 && v < 0) {
				zeroCrossings++
			}
		}
	}
	meanSquare := sumSquares / int64(n)
	rms := Q15(isqrt64(uint64(meanSquare)))
	zcr := Q15((int64(zeroCrossings) * Q15Scale) / int64(n-1))

	candidates, voiced := autocorrelationCandidates(samples, sampleRate)
	if rms < 120 {
		voiced = false
		for i := range candidates {
			if candidates[i].ConfidenceQ15 > 6000 {
				candidates[i].ConfidenceQ15 = 6000
			}
		}
	}
	return Features{RMSQ15: rms, ZCRCount: zeroCrossings, ZCRQ15: zcr, Voiced: voiced, F0Candidates: candidates}
}

func autocorrelationCandidates(samples []int16, sampleRate int) ([]F0Candidate, bool) {
	n := len(samples)
	minLag := sampleRate / 320 // highest F0 about 320 Hz
	if minLag < 2 {
		minLag = 2
	}
	maxLag := n * 3 / 4
	if declared := sampleRate / 65; declared > 0 && declared < maxLag {
		maxLag = declared // lowest F0 about 65 Hz
	}
	if maxLag <= minLag+2 {
		return nil, false
	}

	energy := int64(0)
	for _, s := range samples {
		v := int64(s)
		energy += v * v
	}
	if energy == 0 {
		return nil, false
	}

	type peak struct {
		lag  int
		corr int64
	}
	var peaks []peak
	corrAt := func(lag int) int64 {
		var corr int64
		for i := 0; i+lag < n; i++ {
			corr += int64(samples[i]) * int64(samples[i+lag])
		}
		return corr
	}
	for lag := minLag; lag <= maxLag; lag++ {
		corr := corrAt(lag)
		prev := corrAt(lag - 1)
		next := corrAt(lag + 1)
		if corr > prev && corr >= next && corr > 0 {
			peaks = append(peaks, peak{lag: lag, corr: corr})
		}
	}
	if len(peaks) == 0 {
		bestLag, bestCorr := minLag, int64(-1)
		for lag := minLag; lag <= maxLag; lag++ {
			corr := corrAt(lag)
			if corr > bestCorr {
				bestCorr, bestLag = corr, lag
			}
		}
		peaks = append(peaks, peak{bestLag, bestCorr})
	}
	sort.SliceStable(peaks, func(i, j int) bool { return peaks[i].corr > peaks[j].corr })

	candidates := make([]F0Candidate, 0, 3)
	usedLag := map[int]bool{}
	for _, p := range peaks {
		if usedLag[p.lag] {
			continue
		}
		duplicate := false
		for lag := range usedLag {
			d := lag - p.lag
			if d < 0 {
				d = -d
			}
			if d < 4 {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		usedLag[p.lag] = true
		confidence := Q15((p.corr * Q15Scale) / energy)
		if confidence < 0 {
			confidence = 0
		}
		if int64(confidence) > Q15Scale {
			confidence = Q15(Q15Scale)
		}
		hz := Q8((int64(sampleRate) * Q8Scale) / int64(p.lag))
		candidates = append(candidates, F0Candidate{HzQ8: hz, LagSamples: p.lag, ConfidenceQ15: confidence})
		if len(candidates) == 3 {
			break
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].ConfidenceQ15 == candidates[j].ConfidenceQ15 {
			return candidates[i].LagSamples < candidates[j].LagSamples
		}
		return candidates[i].ConfidenceQ15 > candidates[j].ConfidenceQ15
	})
	voiced := len(candidates) > 0 && candidates[0].ConfidenceQ15 >= VoicingThresholdQ15
	return candidates, voiced
}

// FramePhonemeWeight returns the deterministic loudness weight used by a
// frame. If several phonemes overlap a boundary frame, the maximum weight is
// used; coverage validation makes the overlap explicit.
func FramePhonemeWeight(p *Project, f Frame) Q15 {
	weight := Q15(0)
	for _, id := range f.PhonemeIDs {
		ph := phonemeByID(p, id)
		if ph != nil && ph.WeightQ15 > 0 && ph.WeightQ15 > weight {
			weight = ph.WeightQ15
		}
	}
	if weight == 0 {
		return PermilleToQ15(1000)
	}
	return weight
}
