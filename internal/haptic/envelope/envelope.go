package envelope

import (
	"sort"

	"github.com/liang/hapticfeedback/internal/haptic/detrand"
	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
)

func DefaultRuleSnapshot(capability model.Capability) model.RuleSnapshot {
	return model.RuleSnapshot{
		RuleVersion:         model.RuleVersion,
		AlgorithmVersion:    model.AlgorithmVersion,
		SamplePeriodMS:      capability.SamplePeriodMS,
		QuantumLevel:        capability.QuantumLevel,
		MaxAmplitude:        capability.MaxAmplitudeLevel,
		MaxSamplesPerPacket: capability.MaxSamplesPerPacket,
		PacketWindowMS:      capability.PacketWindowMS,
		FixedScale:          model.FixedScale,
		Templates: map[string][]int{
			"click":  {1000, 420, 180, 60, 0, 0, 0, 0},
			"wave":   {250, 600, 900, 1000, 900, 600, 250, 80},
			"rumble": {650, 900, 1000, 950, 900, 820, 720, 600},
		},
	}
}

func ValidateOrder(events []model.Event) error {
	for i, ev := range events {
		if i == 0 {
			continue
		}
		prev := events[i-1]
		if ev.StartSample < prev.StartSample {
			e := errs.New(errs.EOrder, "frame timing goes backwards")
			e.Path = "$.frames[" + itoa(i) + "].at"
			e.FrameNo = ev.FrameNo
			e.Channel = ev.Channel
			e.Field = "at"
			e.Details = map[string]any{
				"previous_frame_no":     prev.FrameNo,
				"previous_start_sample": prev.StartSample,
				"current_start_sample":  ev.StartSample,
			}
			return e
		}
		if ev.FrameNo < prev.FrameNo {
			e := errs.New(errs.EOrder, "frame number goes backwards")
			e.Path = "$.frames[" + itoa(i) + "].frame_no"
			e.FrameNo = ev.FrameNo
			e.Channel = ev.Channel
			e.Details = map[string]any{"previous_frame_no": prev.FrameNo, "current_frame_no": ev.FrameNo}
			return e
		}
	}
	return nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	n := len(b)
	for v > 0 {
		n--
		b[n] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

func EventChannels(ev model.Event) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, ch := range ev.ChannelSequence {
		if ch != "" && !seen[ch] {
			seen[ch] = true
			out = append(out, ch)
		}
	}
	if len(out) == 0 && ev.Channel != "" {
		out = append(out, ev.Channel)
	}
	return out
}

type Span struct {
	Event    model.Event
	Strength int
	Start    int
	End      int
	Jitter   int
}

type ChannelResult struct {
	Channel   string
	Length    int
	Frames    []model.Frame
	Decisions []model.Decision
	Spans     []Span
}

func eventsForChannel(events []model.Event, channel string) []model.Event {
	out := []model.Event{}
	for _, ev := range events {
		for _, ch := range EventChannels(ev) {
			if ch == channel {
				out = append(out, ev)
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ArrivalOrder != out[j].ArrivalOrder {
			return out[i].ArrivalOrder < out[j].ArrivalOrder
		}
		return out[i].FrameNo < out[j].FrameNo
	})
	return out
}

func SynthesizeChannel(stream model.Stream, channel string, capability model.Capability, rules model.RuleSnapshot) ChannelResult {
	ordered := eventsForChannel(stream.Events, channel)
	decisions := []model.Decision{}
	spans := []Span{}
	length := ChannelLength(stream.Events, channel, capability)
	for _, ev := range ordered {
		jitter := detrand.JitterPermille(stream.Seed, ev.FrameNo, ev.JitterAmplitude)
		if ev.Conditional && !ev.Flag {
			decisions = append(decisions, model.Decision{
				FrameNo: ev.FrameNo, EventID: ev.EventID, Channel: channel,
				Action: "dropped_inactive", Reason: "conditional flag is false",
				StartSample: ev.StartSample, EndSample: ev.StartSample + ev.DurationSamples,
				JitterPermille: jitter,
			})
			continue
		}

		start := ev.StartSample
		end := start + ev.DurationSamples
		merged := false
		if ev.MergeKey != "" {
			for i := range spans {
				sp := &spans[i]
				if sp.Event.MergeKey == ev.MergeKey && start < sp.End && sp.Start < end {
					oldEnd := sp.End
					if end > sp.End {
						sp.End = end
					}
					if ev.Strength > sp.Strength {
						sp.Strength = ev.Strength
					}
					decisions = append(decisions, model.Decision{
						FrameNo: ev.FrameNo, EventID: ev.EventID, Channel: channel,
						Action: "merged", Reason: "same merge_key and overlapping interval; union span and max strength",
						AgainstFrame: sp.Event.FrameNo, StartSample: start, EndSample: end,
						EffectiveFrom: sp.Start, EffectiveTo: sp.End, JitterPermille: jitter,
					})
					_ = oldEnd
					merged = true
					break
				}
			}
		}
		if merged {
			continue
		}

		blocked := false
		for _, sp := range spans {
			if !overlap(start, end, sp.Start, sp.End) {
				continue
			}
			if sp.Event.NoInterrupt {
				decisions = append(decisions, dropDecision(ev, channel, "dropped_protected", "earlier event has no_interrupt=true", sp.Event.FrameNo, start, end, jitter))
				blocked = true
				break
			}
			if sp.Event.Priority > ev.Priority {
				decisions = append(decisions, dropDecision(ev, channel, "dropped_priority", "overlapping event has higher priority", sp.Event.FrameNo, start, end, jitter))
				blocked = true
				break
			}
			if sp.Event.Priority == ev.Priority {
				decisions = append(decisions, dropDecision(ev, channel, "dropped_arrival", "equal priority is resolved by earlier arrival order", sp.Event.FrameNo, start, end, jitter))
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}

		next := make([]Span, 0, len(spans)+1)
		for _, sp := range spans {
			if !overlap(start, end, sp.Start, sp.End) {
				next = append(next, sp)
				continue
			}
			if sp.Event.Priority < ev.Priority && !sp.Event.NoInterrupt {
				if start <= sp.Start {
					decisions = append(decisions, model.Decision{
						FrameNo: sp.Event.FrameNo, EventID: sp.Event.EventID, Channel: channel,
						Action: "replaced", Reason: "higher priority event replaces lower-priority overlap",
						AgainstFrame: ev.FrameNo, StartSample: sp.Start, EndSample: sp.End,
						JitterPermille: sp.Jitter,
					})
					continue
				}
				oldEnd := sp.End
				sp.End = start
				decisions = append(decisions, model.Decision{
					FrameNo: sp.Event.FrameNo, EventID: sp.Event.EventID, Channel: channel,
					Action: "truncated", Reason: "lower priority event truncated by higher-priority overlap",
					AgainstFrame: ev.FrameNo, StartSample: sp.Start, EndSample: oldEnd,
					EffectiveFrom: sp.Start, EffectiveTo: sp.End, JitterPermille: sp.Jitter,
				})
			}
			next = append(next, sp)
		}
		spans = append(next, Span{Event: ev, Strength: ev.Strength, Start: start, End: end, Jitter: jitter})
		decisions = append(decisions, model.Decision{
			FrameNo: ev.FrameNo, EventID: ev.EventID, Channel: channel,
			Action: "effective", Reason: "accepted by deterministic overlap rules",
			StartSample: start, EndSample: end, EffectiveFrom: start, EffectiveTo: end,
			JitterPermille: jitter,
		})
	}

	rawQ8 := make([]int64, length)
	for _, sp := range spans {
		pattern := rules.Templates[sp.Event.Template]
		duration := sp.End - sp.Start
		for offset := 0; offset < duration; offset++ {
			abs := sp.Start + offset
			if abs < 0 || abs >= length {
				continue
			}
			factor := TemplateFactor(pattern, offset)
			adjusted := factor * int64(1000+sp.Jitter) / 1000
			if adjusted < 0 {
				adjusted = 0
			}
			if adjusted > 1200 {
				adjusted = 1200
			}
			rawQ8[abs] += int64(sp.Strength) * model.FixedScale * adjusted / 1000
		}
	}

	quantumQ8 := int64(capability.QuantumLevel) * model.FixedScale
	maxQ8 := int64(capability.MaxAmplitudeLevel) * model.FixedScale
	frames := make([]model.Frame, length)
	for i, raw := range rawQ8 {
		q := raw
		if quantumQ8 > 0 {
			q = (raw + quantumQ8/2) / quantumQ8 * quantumQ8
		}
		if q > maxQ8 {
			q = maxQ8
		}
		frames[i] = model.Frame{SampleIndex: i, FrameQ8: q, Level: int(q / model.FixedScale)}
	}

	return ChannelResult{Channel: channel, Length: length, Frames: frames, Decisions: decisions, Spans: spans}
}

func ChannelLength(events []model.Event, channel string, capability model.Capability) int {
	length := capability.TailSamples
	for _, ev := range events {
		for _, ch := range EventChannels(ev) {
			if ch == channel {
				if candidate := ev.StartSample + ev.DurationSamples + capability.TailSamples; candidate > length {
					length = candidate
				}
			}
		}
	}
	return length
}

func TemplateFactor(pattern []int, offset int) int64 {
	if len(pattern) == 0 || offset < 0 {
		return 0
	}
	if offset < len(pattern) {
		return int64(pattern[offset])
	}
	return int64(pattern[len(pattern)-1])
}

func overlap(startA, endA, startB, endB int) bool {
	return startA < endB && startB < endA
}

func dropDecision(ev model.Event, channel, action, reason string, against, start, end, jitter int) model.Decision {
	return model.Decision{
		FrameNo: ev.FrameNo, EventID: ev.EventID, Channel: channel, Action: action, Reason: reason,
		AgainstFrame: against, StartSample: start, EndSample: end, JitterPermille: jitter,
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
