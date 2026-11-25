package reference

import (
	"fmt"
	"sort"

	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
)

type Violation struct {
	PacketSeq    int    `json:"packet_seq"`
	Item         string `json:"item"`
	SampleOffset int    `json:"sample_offset,omitempty"`
	PacketOffset int    `json:"packet_offset,omitempty"`
	Expected     any    `json:"expected,omitempty"`
	Actual       any    `json:"actual,omitempty"`
	Message      string `json:"message"`
}

func (v Violation) Error() string {
	return fmt.Sprintf("E_DIFF packet=%d item=%s offset=%d: %s", v.PacketSeq, v.Item, v.SampleOffset, v.Message)
}

func (v *Violation) AsError() *errs.Error {
	if v == nil {
		return nil
	}
	e := errs.New(errs.EDiff, v.Message)
	e.Seq = v.PacketSeq
	e.Field = v.Item
	e.Details = map[string]any{
		"packet_seq": v.PacketSeq, "item": v.Item, "sample_offset": v.SampleOffset,
		"packet_offset": v.PacketOffset, "expected": v.Expected, "actual": v.Actual,
	}
	return e
}

type voice struct {
	ev       model.Event
	strength int
	start    int
	end      int
	jitter   int
}

func splitMix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func jitter(seed int64, frame, amplitude int) int {
	if amplitude <= 0 {
		return 0
	}
	r := splitMix(uint64(seed) ^ uint64(frame)*0x9e3779b185ebca87)
	return int(r%uint64(amplitude*2+1)) - amplitude
}

func channels(ev model.Event) []string {
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

func usesChannel(ev model.Event, wanted string) bool {
	for _, ch := range channels(ev) {
		if ch == wanted {
			return true
		}
	}
	return false
}

func RecomputeChannel(events []model.Event, seed int64, channel string, rules model.RuleSnapshot, length int) []model.Frame {
	ordered := make([]model.Event, 0, len(events))
	for _, ev := range events {
		if usesChannel(ev, channel) {
			ordered = append(ordered, ev)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ArrivalOrder != ordered[j].ArrivalOrder {
			return ordered[i].ArrivalOrder < ordered[j].ArrivalOrder
		}
		return ordered[i].FrameNo < ordered[j].FrameNo
	})

	voices := []voice{}
	for _, ev := range ordered {
		j := jitter(seed, ev.FrameNo, ev.JitterAmplitude)
		if ev.Conditional && !ev.Flag {
			continue
		}
		start, end := ev.StartSample, ev.StartSample+ev.DurationSamples
		merged := false
		if ev.MergeKey != "" {
			for i := range voices {
				v := &voices[i]
				if v.ev.MergeKey == ev.MergeKey && start < v.end && v.start < end {
					if end > v.end {
						v.end = end
					}
					if ev.Strength > v.strength {
						v.strength = ev.Strength
					}
					merged = true
					break
				}
			}
		}
		if merged {
			continue
		}
		blocked := false
		for _, v := range voices {
			if start >= v.end || v.start >= end {
				continue
			}
			if v.ev.NoInterrupt || v.ev.Priority > ev.Priority || v.ev.Priority == ev.Priority {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		next := voices[:0]
		for _, v := range voices {
			if start >= v.end || v.start >= end {
				next = append(next, v)
				continue
			}
			if start <= v.start {
				continue
			}
			v.end = start
			next = append(next, v)
		}
		voices = append(next, voice{ev: ev, strength: ev.Strength, start: start, end: end, jitter: j})
	}

	raw := make([]int64, length)
	for _, v := range voices {
		pattern := rules.Templates[v.ev.Template]
		for t := v.start; t < v.end && t < length; t++ {
			if t < 0 {
				continue
			}
			off := t - v.start
			factor := int64(0)
			if off < len(pattern) {
				factor = int64(pattern[off])
			} else if len(pattern) > 0 {
				factor = int64(pattern[len(pattern)-1])
			}
			adjusted := factor * int64(1000+v.jitter) / 1000
			if adjusted < 0 {
				adjusted = 0
			}
			if adjusted > 1200 {
				adjusted = 1200
			}
			raw[t] += int64(v.strength) * rules.FixedScale * adjusted / 1000
		}
	}

	quantum := int64(rules.QuantumLevel) * rules.FixedScale
	maxQ := int64(rules.MaxAmplitude) * rules.FixedScale
	frames := make([]model.Frame, length)
	for i, value := range raw {
		q := value
		if quantum > 0 {
			q = (value + quantum/2) / quantum * quantum
		}
		if q > maxQ {
			q = maxQ
		}
		frames[i] = model.Frame{SampleIndex: i, FrameQ8: q, Level: int(q / rules.FixedScale)}
	}
	return frames
}

func PacketEventCount(pkt model.Packet) int {
	count := 0
	for _, ev := range pkt.Events {
		if !usesChannel(ev, pkt.Channel) {
			continue
		}
		if ev.StartSample >= pkt.StartSample && ev.StartSample < pkt.EndSample {
			count++
		}
	}
	return count
}

func ReviewPacket(pkt model.Packet, expectedFingerprint string) *Violation {
	if got := model.DigestFrames(pkt.Frames); got != pkt.FrameDigest {
		return &Violation{PacketSeq: pkt.GlobalSeq, Item: "frame_digest", Expected: pkt.FrameDigest, Actual: got, Message: "persisted frame digest does not match frames"}
	}
	if got := model.PacketContentHash(pkt); got != pkt.ContentHash {
		return &Violation{PacketSeq: pkt.GlobalSeq, Item: "content_hash", Expected: pkt.ContentHash, Actual: got, Message: "persisted content hash does not match packet content"}
	}
	if expectedFingerprint != "" && pkt.SemanticFingerprint != expectedFingerprint {
		return &Violation{PacketSeq: pkt.GlobalSeq, Item: "semantic_fingerprint", Expected: expectedFingerprint, Actual: pkt.SemanticFingerprint, Message: "truth fingerprint mismatch"}
	}
	if pkt.RuleVersion != model.RuleVersion || pkt.AlgorithmVersion != model.AlgorithmVersion ||
		pkt.RuleSnapshot.RuleVersion != model.RuleVersion || pkt.RuleSnapshot.AlgorithmVersion != model.AlgorithmVersion {
		return &Violation{PacketSeq: pkt.GlobalSeq, Item: "version", Expected: model.RuleVersion + "/" + model.AlgorithmVersion, Actual: pkt.RuleVersion + "/" + pkt.AlgorithmVersion, Message: "rule or algorithm version mismatch"}
	}
	if len(pkt.Frames) != pkt.SampleCount {
		return &Violation{PacketSeq: pkt.GlobalSeq, Item: "sample_count", Expected: pkt.SampleCount, Actual: len(pkt.Frames), Message: "frame slice length mismatch"}
	}
	if pkt.SampleCount > pkt.RuleSnapshot.MaxSamplesPerPacket {
		return &Violation{PacketSeq: pkt.GlobalSeq, Item: "max_samples_per_packet", Expected: pkt.RuleSnapshot.MaxSamplesPerPacket, Actual: pkt.SampleCount, Message: "packet exceeds capability limit"}
	}
	if pkt.TimestampMS < pkt.WindowStartMS || pkt.TimestampMS >= pkt.WindowEndMS {
		return &Violation{PacketSeq: pkt.GlobalSeq, Item: "timing_window", Expected: [2]int64{pkt.WindowStartMS, pkt.WindowEndMS}, Actual: pkt.TimestampMS, Message: "timestamp outside packet window"}
	}
	if gotCount := PacketEventCount(pkt); gotCount != pkt.EventCount {
		return &Violation{PacketSeq: pkt.GlobalSeq, Item: "event_count", Expected: pkt.EventCount, Actual: gotCount, Message: "independent packet event count mismatch"}
	}

	recomputed := RecomputeChannel(pkt.Events, pkt.StreamSeed, pkt.Channel, pkt.RuleSnapshot, pkt.EndSample)
	for offset := 0; offset < pkt.SampleCount; offset++ {
		abs := pkt.StartSample + offset
		if abs >= len(recomputed) {
			return &Violation{PacketSeq: pkt.GlobalSeq, Item: "sample_offset", PacketOffset: offset, SampleOffset: abs, Message: "recomputed envelope ended early"}
		}
		want := recomputed[abs]
		got := pkt.Frames[offset]
		if want.FrameQ8 != got.FrameQ8 || want.Level != got.Level {
			return &Violation{
				PacketSeq: pkt.GlobalSeq, Item: "frame_q8", PacketOffset: offset, SampleOffset: abs,
				Expected: map[string]any{"frame_q8": want.FrameQ8, "level": want.Level},
				Actual:   map[string]any{"frame_q8": got.FrameQ8, "level": got.Level},
				Message:  "independently recomputed envelope sample differs",
			}
		}
	}
	return nil
}

func ReviewRecords(packets []model.Packet, expectedFingerprint string) *Violation {
	for _, pkt := range packets {
		if v := ReviewPacket(pkt, expectedFingerprint); v != nil {
			return v
		}
	}
	return nil
}

func FirstPacketDifference(left, right []model.Packet) *Violation {
	if len(left) != len(right) {
		return &Violation{Item: "packet_count", Expected: len(left), Actual: len(right), Message: "packet sequence length differs"}
	}
	for i := range left {
		a, b := left[i], right[i]
		if a.GlobalSeq != b.GlobalSeq || a.Channel != b.Channel || a.StartSample != b.StartSample || a.SampleCount != b.SampleCount {
			return &Violation{PacketSeq: i, Item: "boundary", Expected: boundaryTuple(a), Actual: boundaryTuple(b), Message: "first unequal packet boundary"}
		}
		if a.EventCount != b.EventCount {
			return &Violation{PacketSeq: a.GlobalSeq, Item: "event_count", Expected: a.EventCount, Actual: b.EventCount, Message: "packet event count differs"}
		}
		limit := minInt(len(a.Frames), len(b.Frames))
		for j := 0; j < limit; j++ {
			if a.Frames[j] != b.Frames[j] {
				return &Violation{
					PacketSeq: a.GlobalSeq, Item: "frame_q8", PacketOffset: j, SampleOffset: a.StartSample + j,
					Expected: a.Frames[j], Actual: b.Frames[j], Message: "first unequal sample",
				}
			}
		}
		if len(a.Frames) != len(b.Frames) {
			return &Violation{PacketSeq: a.GlobalSeq, Item: "frames.length", Expected: len(a.Frames), Actual: len(b.Frames), Message: "frame count differs"}
		}
		if fmt.Sprint(a.Decisions) != fmt.Sprint(b.Decisions) {
			return &Violation{PacketSeq: a.GlobalSeq, Item: "decisions", Expected: a.Decisions, Actual: b.Decisions, Message: "adjudication details differ"}
		}
	}
	return nil
}

func boundaryTuple(p model.Packet) map[string]any {
	return map[string]any{"seq": p.GlobalSeq, "channel": p.Channel, "start": p.StartSample, "count": p.SampleCount}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
