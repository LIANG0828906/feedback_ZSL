package fit

import (
	"sort"

	"prosodyalign/internal/errs"
	"prosodyalign/internal/model"
)

const AlgorithmVersion = "fit-waterfill-v1"

type alloc struct {
	added  []int64
	capped []bool
	cap    []int64
	reason []string
	basis  []map[string]any
}

func effectiveCap(p *model.Project, params model.FitParameters, i int) int64 {
	ph := p.Phonemes[i]
	if ph.MaxStretchSamples != nil {
		return *ph.MaxStretchSamples
	}
	return params.DefaultMaxStretch
}

func Allocate(p *model.Project, params model.FitParameters, accent *model.AccentResult) (*model.FitResult, error) {
	n := len(p.Phonemes)
	lastEnd := p.Phonemes[n-1].EndSample
	gap := params.TargetDurationSamples - lastEnd
	result := &model.FitResult{TargetDuration: params.TargetDurationSamples, Gap: gap, Segments: make([]model.SegmentFit, n)}
	if gap < 0 {
		return result, errs.Fit(n-1, "target duration is shorter than source duration; compression is outside stretch allocator", map[string]any{
			"target_duration": params.TargetDurationSamples, "source_end": lastEnd,
		})
	}

	a := alloc{added: make([]int64, n), capped: make([]bool, n), cap: make([]int64, n), reason: make([]string, n), basis: make([]map[string]any, n)}

	var silenceReserve int64
	for i, ph := range p.Phonemes {
		a.cap[i] = effectiveCap(p, params, i)
		if ph.Kind == model.Silence {
			deficit := params.MinSilenceHoldSamples - ph.DurationSamples()
			if deficit > 0 {
				a.added[i] = deficit
				silenceReserve += deficit
				a.reason[i] = "mandatory_silence_min_hold_deficit"
				a.basis[i] = map[string]any{"minimum": params.MinSilenceHoldSamples, "original_duration": ph.DurationSamples(), "deficit": deficit}
			} else {
				a.reason[i] = "silence_min_hold_preserved"
				a.basis[i] = map[string]any{"minimum": params.MinSilenceHoldSamples, "original_duration": ph.DurationSamples()}
			}
		} else if !ph.Stretchable {
			a.reason[i] = "phoneme_not_marked_stretchable"
		}
	}
	if silenceReserve > gap {
		for i, ph := range p.Phonemes {
			if ph.Kind == model.Silence && ph.DurationSamples() < params.MinSilenceHoldSamples {
				return result, errs.Fit(i, "duration gap cannot reserve minimum silence hold", map[string]any{
					"target_duration":  params.TargetDurationSamples,
					"gap":              gap,
					"required_reserve": silenceReserve,
					"silence_duration": ph.DurationSamples(),
					"minimum_hold":     params.MinSilenceHoldSamples,
				})
			}
		}
	}

	remaining := gap - silenceReserve
	active := make([]int, 0, n)
	var activeWeight int64
	for i, ph := range p.Phonemes {
		if ph.Kind != model.Silence && ph.Stretchable && ph.WeightPer10k > 0 && a.cap[i] > 0 {
			active = append(active, i)
			activeWeight += ph.WeightPer10k
		} else if ph.Kind != model.Silence && ph.Stretchable {
			a.reason[i] = "stretchable_but_zero_weight_or_cap"
		}
	}

	for remaining > 0 && len(active) > 0 {
		type binding struct {
			idx int
			ge  bool
		}
		bindings := make([]int, 0)
		totalWeight := int64(0)
		for _, i := range active {
			totalWeight += p.Phonemes[i].WeightPer10k
		}
		nextActive := make([]int, 0, len(active))
		for _, i := range active {
			w := p.Phonemes[i].WeightPer10k
			capLeft := a.cap[i] - a.added[i]
			if capLeft <= 0 || remaining*w >= capLeft*totalWeight {
				bindings = append(bindings, i)
			} else {
				nextActive = append(nextActive, i)
			}
		}
		if len(bindings) == 0 {
			type share struct {
				idx int
				q   int64
				rem int64
			}
			shares := make([]share, 0, len(active))
			var allocatedFloor int64
			for _, i := range active {
				w := p.Phonemes[i].WeightPer10k
				num := remaining * w
				q := num / totalWeight
				rem := num % totalWeight
				a.added[i] += q
				allocatedFloor += q
				shares = append(shares, share{idx: i, q: q, rem: rem})
			}
			leftover := remaining - allocatedFloor
			sort.Slice(shares, func(i, j int) bool {
				if shares[i].rem != shares[j].rem {
					return shares[i].rem > shares[j].rem
				}
				return shares[i].idx < shares[j].idx
			})
			for k := int64(0); k < leftover && int(k) < len(shares); k++ {
				a.added[shares[k].idx]++
			}
			for _, sh := range shares {
				i := sh.idx
				if a.added[i] >= a.cap[i] {
					a.capped[i] = true
					a.reason[i] = "integer_largest_remainder_reached_bound"
				} else {
					a.reason[i] = "weighted_largest_remainder_share"
				}
				a.basis[i] = map[string]any{
					"remaining_gap_at_stage": remaining, "active_weight_sum": totalWeight,
					"weight": p.Phonemes[i].WeightPer10k, "floor_share": sh.q,
					"fraction_remainder": sh.rem, "integer_samples_added": a.added[i],
					"upper_bound": a.cap[i], "redistribution_rule": "capped_waterfill_then_largest_remainder",
				}
			}
			remaining = 0
			break
		}
		for _, i := range bindings {
			capLeft := a.cap[i] - a.added[i]
			a.added[i] += capLeft
			remaining -= capLeft
			a.capped[i] = true
			a.reason[i] = "upper_bound_triggered_redistribute_remaining_gap"
			a.basis[i] = map[string]any{
				"remaining_gap_at_stage": remaining + capLeft,
				"weight":                 p.Phonemes[i].WeightPer10k,
				"active_weight_sum":      totalWeight,
				"upper_bound":            a.cap[i],
				"bound_sample_space":     capLeft,
				"redistribution_rule":    "fix_segment_at_bound_and_waterfill_remainder",
			}
		}
		active = nextActive
	}

	var allocated int64
	cursor := p.Phonemes[0].StartSample
	for i, ph := range p.Phonemes {
		if i > 0 {
			originalGap := ph.StartSample - p.Phonemes[i-1].EndSample
			if originalGap > 0 {
				cursor += originalGap
			}
		}
		added := a.added[i]
		allocated += added
		newDuration := ph.DurationSamples() + added
		newStart := cursor
		newEnd := cursor + newDuration
		cursor = newEnd
		coef := int64(1000)
		if ph.DurationSamples() > 0 {
			coef = (newDuration*1000 + ph.DurationSamples()/2) / ph.DurationSamples()
		}
		result.Segments[i] = model.SegmentFit{
			PhonemeIndex: i, OriginalStart: ph.StartSample, OriginalEnd: ph.EndSample,
			OriginalDuration: ph.DurationSamples(), AddedSamples: added,
			NewStart: newStart, NewEnd: newEnd, NewDuration: newDuration,
			CoefficientPermille: coef, CapSamples: a.cap[i], CapTriggered: a.capped[i],
			WeightPer10k: ph.WeightPer10k, Reason: a.reason[i],
		}
	}
	result.Allocated = allocated
	result.FinalDuration = cursor
	result.Shortfall = params.TargetDurationSamples - cursor
	result.Feasible = result.Shortfall == 0

	if accent != nil {
		accentCopy := *accent
		accentCopy.ToleranceSamples = params.AccentTolerance
		warped := warpSample(accentCopy.Sample, p, result.Segments)
		result.WarpedAccent = warped
		result.Accent = &accentCopy
		if accentCopy.DistanceSamples > params.AccentTolerance {
			return result, errs.Fit(accentCopy.PhonemeIndex, "loudness and pitch peaks are outside accent tolerance after parameter check", map[string]any{
				"loudness_peak_sample": accentCopy.LoudnessPeak,
				"pitch_peak_sample":    accentCopy.PitchPeak,
				"left_measured":        accentCopy.LoudnessPeak,
				"right_measured":       accentCopy.PitchPeak,
				"measured_distance":    accentCopy.DistanceSamples,
				"tolerance_samples":    params.AccentTolerance,
			})
		}
	}
	if result.Shortfall > 0 {
		return result, errs.Fit(n-1, "stretch upper bounds exhausted before target duration", map[string]any{
			"target_duration": params.TargetDurationSamples,
			"final_duration":  result.FinalDuration,
			"shortfall":       result.Shortfall,
			"allocated":       allocated,
			"redistribution":  "all eligible segments reached bounds or have zero weight",
		})
	}
	return result, nil
}

func warpSample(sample int64, p *model.Project, segs []model.SegmentFit) int64 {
	for i, ph := range p.Phonemes {
		if sample >= ph.StartSample && sample <= ph.EndSample {
			offset := sample - ph.StartSample
			orig := ph.DurationSamples()
			if orig <= 0 || i >= len(segs) {
				return sample
			}
			newDur := segs[i].NewDuration
			return segs[i].NewStart + (offset*newDur+orig/2)/orig
		}
	}
	return sample
}
