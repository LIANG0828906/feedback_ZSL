// Package verify checks synthesized output against generator-provided truth.
package verify

import (
	"fmt"
	"sort"

	"audioforge/internal/engine"
	"audioforge/internal/errs"
	"audioforge/internal/generator"
	"audioforge/internal/model"
)

func Truth(result *engine.Result, truth generator.Truth, capability model.Capability) error {
	frames := make(map[int]model.EnvelopeFrame, len(result.Frames))
	for _, frame := range result.Frames {
		frames[frame.Channel] = frame
	}
	for _, expected := range truth.Channels {
		frame, ok := frames[expected.Channel]
		if !ok {
			return &errs.Error{
				Code:    errs.EDiff,
				Message: "missing channel envelope frame",
				Channel: expected.Channel,
				Field:   "frame",
			}
		}
		if len(frame.Samples) != expected.FrameSamples {
			return &errs.Error{
				Code:         errs.EDiff,
				Message:      fmt.Sprintf("frame length is %d, truth is %d", len(frame.Samples), expected.FrameSamples),
				Channel:      expected.Channel,
				SampleOffset: int64(min(len(frame.Samples), expected.FrameSamples)),
				Field:        "frame_length",
			}
		}
		for _, point := range expected.Samples {
			if point.Offset < 0 || point.Offset >= int64(len(frame.Samples)) {
				return &errs.Error{
					Code:         errs.EDiff,
					Message:      "truth sample point is outside frame",
					Channel:      expected.Channel,
					SampleOffset: point.Offset,
					Field:        "truth_sample",
				}
			}
			if frame.Samples[point.Offset] != point.Value {
				return &errs.Error{
					Code:         errs.EDiff,
					Message:      fmt.Sprintf("sample is %d, truth is %d", frame.Samples[point.Offset], point.Value),
					Channel:      expected.Channel,
					SampleOffset: point.Offset,
					PacketSeq:    packetAt(result.Packets, expected.Channel, point.Offset),
					Field:        "envelope",
				}
			}
		}
	}
	packetMap := make(map[string]model.Packet, len(result.Packets))
	for _, p := range result.Packets {
		packetMap[fmt.Sprintf("%d/%d", p.Channel, p.Seq)] = p
	}
	truthPackets := append([]generator.TruthPacket(nil), truth.Packets...)
	sort.Slice(truthPackets, func(i, j int) bool {
		if truthPackets[i].Channel != truthPackets[j].Channel {
			return truthPackets[i].Channel < truthPackets[j].Channel
		}
		return truthPackets[i].Seq < truthPackets[j].Seq
	})
	if len(result.Packets) != len(truthPackets) {
		return &errs.Error{
			Code:    errs.EDiff,
			Message: fmt.Sprintf("packet count is %d, truth is %d", len(result.Packets), len(truthPackets)),
			Field:   "packet_count",
		}
	}
	for _, expected := range truthPackets {
		actual, ok := packetMap[fmt.Sprintf("%d/%d", expected.Channel, expected.Seq)]
		if !ok {
			return &errs.Error{
				Code:      errs.EDiff,
				Message:   "truth packet is missing",
				Channel:   expected.Channel,
				PacketSeq: expected.Seq,
				Field:     "packet",
			}
		}
		if actual.StartSample != expected.Start || actual.EndSample() != expected.End || actual.TimingMark != expected.TimingMark {
			return &errs.Error{
				Code:         errs.EDiff,
				Message:      "packet boundary differs from truth",
				Channel:      expected.Channel,
				PacketSeq:    expected.Seq,
				SampleOffset: expected.Start,
				Field:        "boundary",
			}
		}
		if int64(len(actual.Samples)) > capability.MaxPacketSamples {
			return &errs.Error{
				Code:      errs.EPack,
				Message:   "packet exceeds truth capability limit",
				Channel:   actual.Channel,
				PacketSeq: actual.Seq,
				Field:     "sample_count",
			}
		}
	}
	return nil
}

func Conditions(events []model.Event, truth generator.Truth) error {
	for _, event := range events {
		expected, ok := truth.Conditions[event.ID]
		if !ok {
			continue
		}
		if event.Active != expected {
			return &errs.Error{
				Code:    errs.EDiff,
				Message: fmt.Sprintf("conditional activation is %v, truth is %v", event.Active, expected),
				EventID: event.ID,
				Field:   "gate",
			}
		}
	}
	return nil
}

func packetAt(packets []model.Packet, channel int, offset int64) int {
	for _, p := range packets {
		if p.Channel == channel && offset >= p.StartSample && offset < p.EndSample() {
			return p.Seq
		}
	}
	return 0
}
