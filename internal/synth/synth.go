package synth

import (
	"sort"

	"audiobus/domain"
	"audiobus/internal/convert"
)

type Result struct {
	Channel     int
	Frames      []int64
	Raw         []int64
	Events      []domain.Event
	TruncatedAt []int64
}

func Synthesize(channel int, total int, events []domain.Event, m *domain.Manifest) (*Result, error) {
	channelEvents := make([]domain.Event, 0, len(events))
	for _, e := range events {
		if e.Channel == channel {
			channelEvents = append(channelEvents, e)
		}
	}
	selected := SelectEvents(channelEvents)
	frames := make([]int64, total)
	raw := make([]int64, total)
	moving := make([]bool, total)
	truncations := []int64{}

	for _, event := range selected {
		duration := event.EffectiveEnd - event.Start
		if duration <= 0 {
			continue
		}
		templateIndex := 0
		if len(event.TypeSequence) > 0 {
			templateIndex = event.TypeSequence[0]
		}
		if templateIndex < 0 || templateIndex >= len(m.Templates[channel]) {
			templateIndex = 0
		}
		template := m.Templates[channel][templateIndex]
		attack := duration * template.AttackPermille / 1024
		release := duration * template.ReleasePermille / 1024
		if attack > duration {
			attack = duration
		}
		if attack+release > duration {
			release = duration - attack
		}
		for offset := int64(0); offset < duration; offset++ {
			pos := event.Start + offset
			if pos < 0 || pos >= int64(total) {
				continue
			}
			var amplitude int64
			switch {
			case attack > 0 && offset < attack:
				amplitude = roundRatio(event.Gain*offset, attack, convert.HalfUp)
				moving[pos] = true
			case release > 0 && offset >= duration-release:
				numeratorIndex := duration - 1 - offset
				amplitude = roundRatio(event.Gain*numeratorIndex, release, convert.HalfUp)
				moving[pos] = true
			default:
				amplitude = event.Gain
			}
			raw[pos] += amplitude
		}
	}

	for pos, amplitude := range raw {
		if amplitude == 0 {
			continue
		}
		if moving[pos] {
			frames[pos] = QuantizeMoving(amplitude, m.QuantUnit)
		} else {
			frames[pos] = QuantizePeak(amplitude, m.QuantUnit)
		}
		if amplitude%m.QuantUnit != 0 {
			truncations = append(truncations, int64(pos))
		}
	}

	return &Result{Channel: channel, Frames: frames, Raw: raw, Events: selected, TruncatedAt: truncations}, nil
}

func SelectEvents(events []domain.Event) []domain.Event {
	candidates := make([]domain.Event, 0, len(events))
	for _, e := range events {
		if e.Conditional && !e.FlagTrue {
			e.ConflictAction = "dropped_condition_false"
			continue
		}
		e.EffectiveEnd = e.Start + e.Duration
		candidates = append(candidates, e)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Start != candidates[j].Start {
			return candidates[i].Start < candidates[j].Start
		}
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		if candidates[i].ArrivalOrder != candidates[j].ArrivalOrder {
			return candidates[i].ArrivalOrder < candidates[j].ArrivalOrder
		}
		return candidates[i].ID < candidates[j].ID
	})

	selected := make([]domain.Event, 0, len(candidates))
	for _, incoming := range candidates {
		overlaps := overlapping(selected, incoming)
		if len(overlaps) == 0 {
			incoming.ConflictAction = "selected"
			selected = append(selected, incoming)
			continue
		}
		blocked := false
		maxPriority := overlaps[0].Priority
		for _, e := range overlaps {
			if e.NoInterrupt {
				blocked = true
			}
			if e.Priority > maxPriority {
				maxPriority = e.Priority
			}
		}
		if blocked {
			incoming.ConflictAction = "dropped_no_interrupt"
			continue
		}
		if incoming.Priority <= maxPriority {
			incoming.ConflictAction = "dropped_priority_or_arrival"
			continue
		}
		for i := range selected {
			if selected[i].Channel != incoming.Channel {
				continue
			}
			if incoming.Start < selected[i].EffectiveEnd && selected[i].Start < incoming.Start {
				selected[i].EffectiveEnd = incoming.Start
				selected[i].ConflictAction = "clipped_by_higher_priority"
			}
		}
		incoming.ConflictAction = "selected_after_clip"
		selected = append(selected, incoming)
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].Start < selected[j].Start })
	return selected
}

func overlapping(selected []domain.Event, e domain.Event) []*domain.Event {
	var out []*domain.Event
	for i := range selected {
		x := &selected[i]
		if x.Channel != e.Channel {
			continue
		}
		if e.Start < x.EffectiveEnd && x.Start < e.Start+e.Duration {
			out = append(out, x)
		}
	}
	return out
}

func roundRatio(numerator, denominator int64, rounding convert.Rounding) int64 {
	if denominator == 0 {
		return 0
	}
	v, _ := convert.ScaleChecked(numerator, 1, denominator, rounding)
	return v
}

func QuantizePeak(value, unit int64) int64 {
	if unit <= 1 {
		return value
	}
	return (value + unit/2) / unit
}

func QuantizeMoving(value, unit int64) int64 {
	if unit <= 1 {
		return value
	}
	return (2*value + unit - 1) / (2 * unit)
}

func Quantize(value, unit int64, mode string) int64 {
	switch mode {
	case "floor":
		return value / unit
	case "ceil":
		return (value + unit - 1) / unit
	default:
		return QuantizePeak(value, unit)
	}
}
