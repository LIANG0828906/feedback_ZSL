// Package synth performs deterministic per-channel event resolution and
// envelope synthesis. Parallelism never affects the canonical output order.
package synth

import (
	"fmt"
	"sort"
	"sync"

	"audioforge/internal/errs"
	"audioforge/internal/model"
	"audioforge/internal/num"
)

type Result struct {
	Frames    []model.EnvelopeFrame
	Decisions []model.PacketDecision
	Workers   int
	Buffer    int
}

type resolvedEvent struct {
	event     model.Event
	start     int64
	end       int64
	duration  int64
	intensity int64
}

func Synthesize(events []model.Event, capability model.Capability, workers, bufferPerWorker int) (Result, error) {
	if err := capability.Validate(); err != nil {
		return Result{}, errs.New(errs.EPack, "invalid capability: %v", err)
	}
	if workers <= 0 {
		workers = 1
	}
	if bufferPerWorker <= 0 {
		bufferPerWorker = 1
	}
	grouped := make([][]model.Event, capability.ChannelCount)
	decisions := make([][]model.PacketDecision, capability.ChannelCount)
	for _, event := range events {
		if event.StartSample < 0 {
			return Result{}, &errs.Error{Code: errs.EVersion, EventID: event.ID, Field: "start", Message: "event start is negative"}
		}
		if event.DurationSamples <= 0 {
			return Result{}, &errs.Error{Code: errs.EVersion, EventID: event.ID, Field: "duration_samples", Message: "event duration must be positive"}
		}
		if event.StartSample > int64(^uint64(0)>>1)-event.DurationSamples {
			return Result{}, errs.Overflow(event.ID, "start+duration", "event end sample overflows int64")
		}
		if event.Intensity < 0 {
			return Result{}, &errs.Error{Code: errs.EVersion, EventID: event.ID, Field: "intensity", Message: "event intensity must not be negative"}
		}
		if len(event.Routes) == 0 {
			return Result{}, &errs.Error{Code: errs.EVersion, EventID: event.ID, Field: "routes", Message: "event has no channel routes"}
		}
		for _, route := range event.Routes {
			if route < 0 || route >= capability.ChannelCount {
				return Result{}, &errs.Error{
					Code:    errs.EPack,
					EventID: event.ID,
					Field:   "routes",
					Channel: route,
					Message: fmt.Sprintf("route %d exceeds channel count %d", route, capability.ChannelCount),
				}
			}
			if event.Active {
				grouped[route] = append(grouped[route], event.Clone())
			} else {
				decisions[route] = append(decisions[route], model.PacketDecision{
					EventID: event.ID,
					Channel: route,
					Action:  "dropped",
					Reason:  "conditional gate is false",
				})
			}
		}
	}

	effectiveWorkers := workers
	if effectiveWorkers > capability.ChannelCount {
		effectiveWorkers = capability.ChannelCount
	}
	if effectiveWorkers < 1 {
		effectiveWorkers = 1
	}
	jobs := make(chan int, effectiveWorkers*bufferPerWorker)
	results := make([]model.EnvelopeFrame, capability.ChannelCount)
	errorsByChannel := make([]error, capability.ChannelCount)
	var wg sync.WaitGroup
	wg.Add(effectiveWorkers)
	for worker := 0; worker < effectiveWorkers; worker++ {
		go func() {
			defer wg.Done()
			for channel := range jobs {
				frame, channelDecisions, err := synthesizeChannel(channel, grouped[channel], capability)
				if err != nil {
					errorsByChannel[channel] = err
					continue
				}
				results[channel] = frame
				decisions[channel] = append(decisions[channel], channelDecisions...)
			}
		}()
	}
	for channelIndex := 0; channelIndex < capability.ChannelCount; channelIndex++ {
		jobs <- channelIndex
	}
	close(jobs)
	wg.Wait()

	for _, err := range errorsByChannel {
		if err != nil {
			return Result{}, err
		}
	}
	out := Result{
		Frames:  make([]model.EnvelopeFrame, 0, capability.ChannelCount),
		Workers: effectiveWorkers,
		Buffer:  effectiveWorkers * bufferPerWorker,
	}
	for channel := 0; channel < capability.ChannelCount; channel++ {
		out.Frames = append(out.Frames, results[channel])
		out.Decisions = append(out.Decisions, decisions[channel]...)
	}
	return out, nil
}

func synthesizeChannel(channel int, events []model.Event, capability model.Capability) (model.EnvelopeFrame, []model.PacketDecision, error) {
	sorted := append([]model.Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].StartSample != sorted[j].StartSample {
			return sorted[i].StartSample < sorted[j].StartSample
		}
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority > sorted[j].Priority
		}
		if sorted[i].SourceOrdinal != sorted[j].SourceOrdinal {
			return sorted[i].SourceOrdinal < sorted[j].SourceOrdinal
		}
		return sorted[i].ID < sorted[j].ID
	})

	accepted := make([]resolvedEvent, 0, len(sorted))
	decisions := make([]model.PacketDecision, 0, len(sorted))
	current := -1
	for _, event := range sorted {
		if !event.Active {
			decisions = append(decisions, model.PacketDecision{
				EventID: event.ID,
				Channel: channel,
				Action:  "dropped",
				Reason:  "conditional gate is false",
			})
			continue
		}
		if current >= 0 && event.StartSample >= accepted[current].end {
			current = -1
		}
		candidate := resolvedEvent{
			event:     event.Clone(),
			start:     event.StartSample,
			end:       event.StartSample + event.DurationSamples,
			duration:  event.DurationSamples,
			intensity: event.Intensity,
		}
		if current < 0 {
			accepted = append(accepted, candidate)
			current = len(accepted) - 1
			decisions = append(decisions, model.PacketDecision{
				EventID: event.ID,
				Channel: channel,
				Action:  "accepted",
				Reason:  "channel idle",
			})
			continue
		}
		active := &accepted[current]
		tooSoon := candidate.start < active.start+capability.MinEventInterval
		switch {
		case active.event.Blocking:
			decisions = append(decisions, model.PacketDecision{
				EventID: event.ID,
				Channel: channel,
				Action:  "dropped",
				Reason:  "active event is non-interruptible",
			})
		case tooSoon && candidate.event.Priority <= active.event.Priority:
			decisions = append(decisions, model.PacketDecision{
				EventID: event.ID,
				Channel: channel,
				Action:  "dropped",
				Reason:  "minimum event interval",
			})
		case candidate.event.Priority > active.event.Priority:
			decisions = append(decisions, model.PacketDecision{
				EventID: active.event.ID,
				Channel: channel,
				Action:  "truncated",
				Reason:  "preempted by " + candidate.event.ID,
			})
			active.end = candidate.start
			accepted = append(accepted, candidate)
			current = len(accepted) - 1
			decisions = append(decisions, model.PacketDecision{
				EventID: event.ID,
				Channel: channel,
				Action:  "accepted",
				Reason:  "higher priority",
			})
		case candidate.event.Priority < active.event.Priority:
			decisions = append(decisions, model.PacketDecision{
				EventID: event.ID,
				Channel: channel,
				Action:  "dropped",
				Reason:  "lower priority",
			})
		default:
			decisions = append(decisions, model.PacketDecision{
				EventID: event.ID,
				Channel: channel,
				Action:  "dropped",
				Reason:  "arrival order (equal priority)",
			})
		}
	}

	frameEnd := int64(0)
	for _, event := range accepted {
		if event.end > frameEnd {
			frameEnd = event.end
		}
	}
	if frameEnd > int64(^uint(0)>>1) {
		return model.EnvelopeFrame{}, nil, errs.Overflow("", "start+duration", "envelope length exceeds platform capacity")
	}
	raw := make([]int64, int(frameEnd))
	template := capability.Channels[channel].Template
	if len(template) == 0 {
		return model.EnvelopeFrame{}, nil, errs.New(errs.EPack, "channel %d has no envelope template", channel)
	}
	for _, event := range accepted {
		spanStart := int(event.start)
		spanEnd := int(event.end)
		if spanEnd > len(raw) {
			spanEnd = len(raw)
		}
		for pos := spanStart; pos < spanEnd; pos++ {
			relative := int64(pos) - event.start
			idx := int64(0)
			if event.duration > 1 {
				scaledPosition, ok := num.CheckedMul(relative, int64(len(template)-1))
				if !ok {
					return model.EnvelopeFrame{}, nil, errs.Overflow(event.event.ID, "duration_samples", "overflow mapping sample to envelope template")
				}
				idx = scaledPosition / (event.duration - 1)
			} else {
				idx = int64((len(template) - 1) / 2)
			}
			if idx < 0 {
				idx = 0
			}
			if idx >= int64(len(template)) {
				idx = int64(len(template) - 1)
			}
			scaled, ok := num.CheckedMul(template[idx], event.intensity)
			if !ok {
				return model.EnvelopeFrame{}, nil, errs.Overflow(event.event.ID, "intensity", "overflow applying envelope template")
			}
			product, ok := num.CheckedMul(scaled/1000, capability.Channels[channel].GainPermille)
			if !ok {
				return model.EnvelopeFrame{}, nil, errs.Overflow(event.event.ID, "gain_permille", "overflow applying channel gain")
			}
			next := raw[pos] + product/1000
			if (product >= 0 && next < raw[pos]) || (product < 0 && next > raw[pos]) {
				return model.EnvelopeFrame{}, nil, errs.Overflow(event.event.ID, "intensity", "overflow accumulating envelope samples")
			}
			raw[pos] = next
		}
	}
	samples := make([]int64, len(raw))
	for i, value := range raw {
		quantized, ok := num.Quantize(value, capability.QuantUnit, capability.QuantRounding)
		if !ok {
			return model.EnvelopeFrame{}, nil, errs.Overflow("", "quant_unit", "invalid quantization unit")
		}
		samples[i] = quantized
	}
	spans := make([]model.EventSpan, 0, len(accepted))
	for _, event := range accepted {
		spans = append(spans, model.EventSpan{
			EventID:  event.event.ID,
			Start:    event.start,
			End:      event.end,
			Priority: event.event.Priority,
		})
	}
	return model.EnvelopeFrame{Channel: channel, Samples: samples, Spans: spans}, decisions, nil
}

// SynthesizeSerial is useful for determinism tests and small replays.
func SynthesizeSerial(events []model.Event, capability model.Capability) (Result, error) {
	return Synthesize(events, capability, 1, 1)
}
