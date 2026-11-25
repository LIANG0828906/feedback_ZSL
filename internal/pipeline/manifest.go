package pipeline

import (
	"fmt"

	"prosodyalign/internal/model"
)

func phonemePtr(i int) *int { return &i }

func (e *Engine) appendEnvelopeManifest(m *InvalidationManifest, p *model.Project, fromCache bool, reused map[int]string, initial bool) {
	for i := range p.Phonemes {
		item := InvalidationItem{DialogID: p.ID, PhonemeIndex: phonemePtr(i), NodeKind: "phoneme_envelope"}
		if fromCache {
			item.Reused = true
			item.Reason = "persistent_envelope_cache_hit"
			item.ReusedFromKey = reused[i]
			m.ReusedItems = append(m.ReusedItems, item)
		} else {
			item.Reason = "envelope_extraction_failed_or_cache_miss"
			item.Scope = fmt.Sprintf("phoneme:%d envelope frames", i)
			m.Items = append(m.Items, item)
		}
	}
}

func (e *Engine) appendDiffManifest(m *InvalidationManifest, p *model.Project, old, next *model.DialogResult, edit Edit, envFromCache bool, envReusedKeys, stretchCacheHits, rangeCacheHits map[int]string) {
	initial := old == nil
	for i := range p.Phonemes {
		envItem := InvalidationItem{DialogID: p.ID, PhonemeIndex: phonemePtr(i), NodeKind: "phoneme_envelope"}
		if envFromCache {
			envItem.Reused = true
			if key := envReusedKeys[i]; key != "" {
				envItem.Reason = "phoneme_weight_edit_does_not_change_pcm_envelope_reused_persisted_result"
				envItem.ReusedFromKey = key
			} else {
				envItem.Reason = "envelope_content_and_parameter_version_unchanged"
			}
			m.ReusedItems = append(m.ReusedItems, envItem)
		} else {
			envItem.Reason = "initial_extraction"
			envItem.Scope = fmt.Sprintf("phoneme:%d envelope frames", i)
			m.Items = append(m.Items, envItem)
		}
	}

	accentIndex := next.Accent.PhonemeIndex
	accentItem := InvalidationItem{DialogID: p.ID, PhonemeIndex: phonemePtr(accentIndex), NodeKind: "accent_landing"}
	switch {
	case initial:
		accentItem.Reason = "initial_accent_localization"
		accentItem.Scope = "vowel envelope peaks + F0 peak -> accent sample"
		m.Items = append(m.Items, accentItem)
	case edit.AccentTolerance != nil:
		accentItem.Reason = "accent_tolerance_changed"
		accentItem.Scope = "accent validation -> fit accent gate"
		m.Items = append(m.Items, accentItem)
	case old.Accent.Sample != next.Accent.Sample || old.Accent.PitchPeak != next.Accent.PitchPeak:
		accentItem.Reason = "upstream_envelope_or_f0_changed"
		accentItem.Scope = "accent sample and tolerance gate"
		m.Items = append(m.Items, accentItem)
	default:
		accentItem.Reused = true
		accentItem.Reason = "accent_peaks_unchanged"
		m.ReusedItems = append(m.ReusedItems, accentItem)
	}

	for i := range next.Fit.Segments {
		newSeg := next.Fit.Segments[i]
		stretchItem := InvalidationItem{DialogID: p.ID, PhonemeIndex: phonemePtr(i), NodeKind: "stretch_coefficient"}
		rangeItem := InvalidationItem{DialogID: p.ID, PhonemeIndex: phonemePtr(i), NodeKind: "stretched_range"}
		stretchChanged := initial
		rangeChanged := initial
		var oldSeg model.SegmentFit
		if old != nil && i < len(old.Fit.Segments) {
			oldSeg = old.Fit.Segments[i]
			stretchChanged = oldSeg.AddedSamples != newSeg.AddedSamples || oldSeg.CapTriggered != newSeg.CapTriggered || oldSeg.CoefficientPermille != newSeg.CoefficientPermille
			rangeChanged = oldSeg.NewStart != newSeg.NewStart || oldSeg.NewEnd != newSeg.NewEnd
		}

		if stretchChanged {
			stretchItem.Reason = changeReason(edit, initial)
			stretchItem.Scope = fmt.Sprintf("waterfill coefficient p%d -> serial ranges p%d onward", i, i)
			m.Items = append(m.Items, stretchItem)
		} else {
			stretchItem.Reused = true
			stretchItem.Reason = "allocated_samples_and_bound_flag_unchanged"
			stretchItem.ReusedFromKey = stretchCacheHits[i]
			m.ReusedItems = append(m.ReusedItems, stretchItem)
		}

		if rangeChanged {
			if stretchChanged {
				rangeItem.Reason = "own_stretch_changed"
			} else {
				rangeItem.Reason = "upstream_serial_boundary_shift"
			}
			rangeItem.Scope = fmt.Sprintf("range p%d and all later starts; warped accent", i)
			m.Items = append(m.Items, rangeItem)
		} else {
			rangeItem.Reused = true
			rangeItem.Reason = "start_and_end_samples_unchanged"
			rangeItem.ReusedFromKey = rangeCacheHits[i]
			m.ReusedItems = append(m.ReusedItems, rangeItem)
		}
	}

	warpItem := InvalidationItem{DialogID: p.ID, PhonemeIndex: phonemePtr(next.Accent.PhonemeIndex), NodeKind: "warped_accent"}
	if initial || (old != nil && old.Fit.WarpedAccent != next.Fit.WarpedAccent) {
		warpItem.Reason = "stretched_range_changed"
		warpItem.Scope = "accent warp sample"
		m.Items = append(m.Items, warpItem)
	} else {
		warpItem.Reused = true
		warpItem.Reason = "warped_accent_sample_unchanged"
		m.ReusedItems = append(m.ReusedItems, warpItem)
	}
}

func changeReason(edit Edit, initial bool) string {
	if initial {
		return "initial_allocation"
	}
	switch {
	case edit.TargetDurationSamples != nil:
		return "target_duration_changed_recompute_waterfill"
	case edit.DefaultMaxStretch != nil:
		return "stretch_upper_bound_changed_redistribute_gap"
	case edit.PhonemeWeight != nil:
		return "phoneme_weight_changed_redistribute_gap"
	case edit.AccentTolerance != nil:
		return "tolerance_only_does_not_change_coefficients"
	default:
		return "allocation_inputs_changed"
	}
}
