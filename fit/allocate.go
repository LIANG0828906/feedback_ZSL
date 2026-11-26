package fit

import (
	"sort"

	"dialoguefit/model"
)

const AlgorithmVersion = "weighted-waterfall-v1"

type allocation struct {
	added []int
	hit   []bool
	basis []string
}

// Allocate expands a dialogue to Params.TargetSamples. Silence deficits are
// reserved first. The remaining gap is distributed in descending loudness
// weight order; each phoneme is filled up to its integer upper bound, then the
// unresolved gap is passed to the next phoneme. This is the explainable
// weighted-waterfall degradation rule.
func Allocate(d model.Dialogue) ([]model.StretchSegment, error) {
	params := d.Params
	if params.TargetSamples <= 0 {
		return nil, model.FitError(d.ID, -1, "params.target_samples", "target duration must be positive")
	}
	if params.MaxStretch <= 0 {
		return nil, model.FitError(d.ID, -1, "params.max_stretch", "stretch upper bound must be positive")
	}
	for i := range d.Phonemes {
		factor := 1.0
		if factor <= 0 {
			return nil, model.FitError(d.ID, i, "stretch_factor", "stretch coefficient must be positive")
		}
	}

	sourceTotal := d.PCM.SampleCount
	if sourceTotal <= 0 {
		sourceTotal = d.Phonemes[len(d.Phonemes)-1].EndSample
	}
	gap := params.TargetSamples - sourceTotal
	if gap < 0 {
		return nil, model.FitError(d.ID, -1, "target_samples", "target=%d is shorter than source=%d; this allocator only stretches", params.TargetSamples, sourceTotal)
	}

	n := len(d.Phonemes)
	a := &allocation{added: make([]int, n), hit: make([]bool, n), basis: make([]string, n)}

	silenceReserve := 0
	for i, ph := range d.Phonemes {
		if ph.Category != model.CategorySilence {
			continue
		}
		deficit := params.MinSilenceSamples - ph.Duration()
		if deficit > 0 {
			a.added[i] = deficit
			silenceReserve += deficit
			a.basis[i] = "silence-hold-minimum"
		} else {
			a.basis[i] = "silence-preserved"
		}
	}
	if silenceReserve > gap {
		return nil, model.FitError(d.ID, firstDeficitSilence(d), "min_silence_samples", "silence reserve=%d exceeds duration gap=%d", silenceReserve, gap)
	}

	remaining := gap - silenceReserve
	order := make([]int, 0, n)
	for i, ph := range d.Phonemes {
		if ph.Category == model.CategorySilence {
			continue
		}
		if !ph.Stretchable {
			a.basis[i] = "phoneme-not-stretchable"
			continue
		}
		if ph.LoudnessWeight <= 0 {
			a.basis[i] = "zero-weight-not-allocated"
			continue
		}
		order = append(order, i)
	}
	sort.SliceStable(order, func(i, j int) bool {
		pi, pj := d.Phonemes[order[i]], d.Phonemes[order[j]]
		if pi.LoudnessWeight != pj.LoudnessWeight {
			return pi.LoudnessWeight > pj.LoudnessWeight
		}
		return pi.Index < pj.Index
	})

	for _, i := range order {
		ph := d.Phonemes[i]
		if remaining == 0 {
			a.basis[i] = "weighted-waterfall-no-gap"
			continue
		}
		capExtra := int(float64(ph.Duration()) * (params.MaxStretch - 1.0))
		if capExtra <= 0 {
			a.basis[i] = "weighted-waterfall-bound-nonpositive"
			continue
		}
		take := minInt(capExtra, remaining)
		a.added[i] = take
		remaining -= take
		if take >= capExtra {
			a.hit[i] = true
			a.basis[i] = "weighted-waterfall-cap-hit"
		} else {
			a.basis[i] = "weighted-waterfall-partial"
		}
	}
	for i := range d.Phonemes {
		if a.basis[i] == "" {
			a.basis[i] = "weighted-waterfall-no-gap"
		}
	}
	if remaining > 0 {
		return nil, model.FitError(d.ID, d.Phonemes[n-1].Index, "target_samples", "target unreachable after upper bounds; shortfall=%d gap=%d", remaining, gap)
	}

	segments := make([]model.StretchSegment, n)
	cursor := 0
	for i, ph := range d.Phonemes {
		if i == 0 {
			cursor = ph.StartSample
		} else {
			cursor += ph.StartSample - d.Phonemes[i-1].EndSample
		}
		newDuration := ph.Duration() + a.added[i]
		start := cursor
		end := start + newDuration
		cursor = end
		factor := float64(newDuration) / float64(ph.Duration())
		if factor <= 0 {
			return nil, model.FitError(d.ID, i, "stretch_factor", "computed non-positive coefficient %v", factor)
		}
		segments[i] = model.StretchSegment{
			PhonemeID:     ph.ID,
			Index:         ph.Index,
			SourceStart:   ph.StartSample,
			SourceEnd:     ph.EndSample,
			StartSample:   start,
			EndSample:     end,
			StretchFactor: factor,
			AddedSamples:  a.added[i],
			HitCap:        a.hit[i],
			Basis:         a.basis[i],
		}
	}
	return segments, nil
}

func firstDeficitSilence(d model.Dialogue) int {
	for _, ph := range d.Phonemes {
		if ph.Category == model.CategorySilence && ph.Duration() < d.Params.MinSilenceSamples {
			return ph.Index
		}
	}
	return -1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ValidatePositiveCoefficient is a small integration guard for callers that
// supply externally solved coefficients.
func ValidatePositiveCoefficient(dialogueID string, index int, coefficient float64) error {
	if coefficient <= 0 {
		return model.FitError(dialogueID, index, "stretch_factor", "coefficient must be positive, got %v", coefficient)
	}
	return nil
}
