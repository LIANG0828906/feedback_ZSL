package pipeline

import (
	"sort"

	"github.com/liang/hapticfeedback/internal/haptic/boundary"
	"github.com/liang/hapticfeedback/internal/haptic/concurrency"
	"github.com/liang/hapticfeedback/internal/haptic/detrand"
	"github.com/liang/hapticfeedback/internal/haptic/envelope"
	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
)

type Plan struct {
	Stream     model.Stream
	Capability model.Capability
	Packets    []model.Packet
	Decisions  []model.Decision
}

type planItem struct {
	boundary boundary.Boundary
	result   envelope.ChannelResult
}

func BuildPlan(stream *model.Stream, capability model.Capability, sourceBranch int) (*Plan, error) {
	if stream == nil {
		return nil, errs.New(errs.EDecode, "nil stream")
	}
	if err := envelope.ValidateOrder(stream.Events); err != nil {
		return nil, err
	}
	rules := envelope.DefaultRuleSnapshot(capability)
	channels := model.SortedChannels(capability.Channels)
	pool := concurrency.NewPool(capability.MaxWorkers, capability.PerThreadBufferSamples)
	futures := map[string]*concurrency.Future{}
	for _, channel := range channels {
		channel := channel
		length := envelope.ChannelLength(stream.Events, channel, capability)
		futures[channel] = pool.Submit(concurrency.Job{
			Name: "synthesize-" + channel, Samples: length, LaneKey: "channel:" + channel,
			Fn: func() (any, error) {
				return envelope.SynthesizeChannel(*stream, channel, capability, rules), nil
			},
		})
	}

	results := map[string]envelope.ChannelResult{}
	decisions := []model.Decision{}
	for _, channel := range channels {
		value, err := futures[channel].Wait()
		if err != nil {
			return nil, err
		}
		futures[channel].Release()
		result := value.(envelope.ChannelResult)
		results[channel] = result
		decisions = append(decisions, result.Decisions...)
	}
	pool.Shutdown()

	items := []planItem{}
	for _, channel := range channels {
		result := results[channel]
		for _, b := range boundary.PlanChannel(channel, result.Length, capability) {
			items = append(items, planItem{boundary: b, result: result})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].boundary, items[j].boundary
		if a.StartSample != b.StartSample {
			return a.StartSample < b.StartSample
		}
		if a.Channel != b.Channel {
			return a.Channel < b.Channel
		}
		return a.PacketIndex < b.PacketIndex
	})

	fingerprint := stream.Fingerprint()
	packets := make([]model.Packet, 0, len(items))
	for seq, item := range items {
		b := item.boundary
		frames := append([]model.Frame(nil), item.result.Frames[b.StartSample:b.EndSample]...)
		consumed := consumedOffset(stream.Events, b.EndSample)
		eventCount := channelEventCount(stream.Events, b.Channel, b.StartSample, b.EndSample)
		effectiveCount := effectiveDecisionCount(item.result.Decisions, b.StartSample, b.EndSample)
		pkt := model.Packet{
			GlobalSeq: seq, Channel: b.Channel, PacketIndex: b.PacketIndex,
			StartSample: b.StartSample, EndSample: b.EndSample, SampleCount: b.SampleCount,
			TimestampMS: b.TimestampMS, WindowStartMS: b.WindowStartMS, WindowEndMS: b.WindowEndMS,
			Frames: frames, FrameDigest: model.DigestFrames(frames),
			ConsumedEventOffset: consumed, EventCount: eventCount, EffectiveCount: effectiveCount,
			StreamSeed: stream.Seed, RNGProgress: detrand.Progress(consumed),
			SemanticFingerprint: fingerprint, RuleVersion: model.RuleVersion,
			AlgorithmVersion: model.AlgorithmVersion, BranchSources: []int{sourceBranch},
			SourceBranch: sourceBranch, RuleSnapshot: rules, Decisions: item.result.Decisions,
			Events: stream.Events,
		}
		pkt.FingerprintDelta = model.FingerprintDelta(pkt)
		pkt.ContentHash = model.PacketContentHash(pkt)
		if err := boundary.Validate(b, capability, seq); err != nil {
			return nil, err
		}
		packets = append(packets, pkt)
	}
	return &Plan{Stream: *stream, Capability: capability, Packets: packets, Decisions: decisions}, nil
}

func consumedOffset(events []model.Event, endSample int) int {
	count := 0
	for _, ev := range events {
		if ev.StartSample < endSample {
			count++
		}
	}
	return count
}

func channelEventCount(events []model.Event, channel string, start, end int) int {
	count := 0
	for _, ev := range events {
		matches := false
		for _, ch := range envelope.EventChannels(ev) {
			if ch == channel {
				matches = true
				break
			}
		}
		if matches && ev.StartSample >= start && ev.StartSample < end {
			count++
		}
	}
	return count
}

func effectiveDecisionCount(decisions []model.Decision, start, end int) int {
	count := 0
	for _, d := range decisions {
		if d.StartSample >= start && d.StartSample < end && (d.Action == "effective" || d.Action == "merged") {
			count++
		}
	}
	return count
}
