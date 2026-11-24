// Package packet deterministically slices quantized channel envelopes into
// device packets and provides a stable wire codec.
package packet

import (
	"encoding/json"
	"fmt"
	"sort"

	"audioforge/internal/errs"
	"audioforge/internal/model"
)

type WireBatch struct {
	Packets []model.Packet `json:"packets"`
}

func Build(frames []model.EnvelopeFrame, capability model.Capability) ([]model.Packet, error) {
	if err := capability.Validate(); err != nil {
		return nil, errs.New(errs.EPack, "invalid capability: %v", err)
	}
	ordered := append([]model.EnvelopeFrame(nil), frames...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Channel < ordered[j].Channel })
	packets := make([]model.Packet, 0)
	for _, frame := range ordered {
		if frame.Channel < 0 || frame.Channel >= capability.ChannelCount {
			return nil, errs.New(errs.EPack, "envelope channel %d is outside capability", frame.Channel)
		}
		channelSeq := 0
		previousStart := int64(0)
		for start := int64(0); start < int64(len(frame.Samples)); {
			end := start + capability.MaxPacketSamples
			if end > int64(len(frame.Samples)) {
				end = int64(len(frame.Samples))
			}
			count := int(end - start)
			if count <= 0 || int64(count) > capability.MaxPacketSamples {
				return nil, errs.Packet(channelSeq, frame.Channel, "sample_count", "packet sample count %d exceeds %d", count, capability.MaxPacketSamples)
			}
			timingMark := start
			if timingMark < capability.TimingWindow.Min || timingMark > capability.TimingWindow.Max {
				return nil, &errs.Error{
					Code:         errs.EPack,
					Message:      fmt.Sprintf("timing mark %d is outside [%d,%d]", timingMark, capability.TimingWindow.Min, capability.TimingWindow.Max),
					PacketSeq:    channelSeq,
					Channel:      frame.Channel,
					Field:        "timing_mark",
					SampleOffset: start,
				}
			}
			samples := append([]int64(nil), frame.Samples[start:end]...)
			eventIDs, offsets := model.SortedEventOffsets(frame.Spans, start, end)
			packet := model.Packet{
				Seq:             channelSeq,
				Channel:         frame.Channel,
				StartSample:     start,
				SampleCount:     count,
				TimingMark:      timingMark,
				PreviousOffset:  previousStart,
				Samples:         samples,
				DependsOnEvents: eventIDs,
				ConsumedOffsets: offsets,
			}
			packet.RefreshHash()
			packets = append(packets, packet)
			previousStart = start
			channelSeq++
			start = end
		}
	}
	sort.Slice(packets, func(i, j int) bool {
		if packets[i].Channel != packets[j].Channel {
			return packets[i].Channel < packets[j].Channel
		}
		return packets[i].Seq < packets[j].Seq
	})
	return packets, nil
}

func Validate(packet model.Packet, capability model.Capability) error {
	if packet.Channel < 0 || packet.Channel >= capability.ChannelCount {
		return &errs.Error{
			Code:      errs.EPack,
			Message:   "packet channel is outside capability",
			PacketSeq: packet.Seq,
			Channel:   packet.Channel,
			Field:     "channel",
		}
	}
	if len(packet.Samples) == 0 {
		return errs.Packet(packet.Seq, packet.Channel, "samples", "packet is empty")
	}
	if int64(len(packet.Samples)) > capability.MaxPacketSamples {
		return errs.Packet(packet.Seq, packet.Channel, "sample_count", "packet has %d samples, limit is %d", len(packet.Samples), capability.MaxPacketSamples)
	}
	if packet.SampleCount != 0 && packet.SampleCount != len(packet.Samples) {
		return errs.Packet(packet.Seq, packet.Channel, "sample_count", "sample_count does not match sample payload")
	}
	if packet.StartSample < 0 {
		return errs.Packet(packet.Seq, packet.Channel, "start_sample", "start sample is negative")
	}
	if packet.TimingMark < capability.TimingWindow.Min || packet.TimingMark > capability.TimingWindow.Max {
		return errs.Packet(packet.Seq, packet.Channel, "timing_mark", "timing mark %d is outside [%d,%d]", packet.TimingMark, capability.TimingWindow.Min, capability.TimingWindow.Max)
	}
	if packet.PreviousOffset < 0 || packet.PreviousOffset > packet.StartSample {
		return errs.Packet(packet.Seq, packet.Channel, "previous_offset", "previous packet offset is invalid")
	}
	if expected := model.HashPacket(packet); packet.ContentHash != "" && expected != packet.ContentHash {
		return &errs.Error{
			Code:      errs.EDiff,
			Message:   "packet content hash mismatch",
			PacketSeq: packet.Seq,
			Channel:   packet.Channel,
			Field:     "content_hash",
		}
	}
	return nil
}

// Encode returns canonical, byte-stable JSON. The content hash is part of the
// envelope but is computed from a stable binary representation.
func Encode(packet model.Packet) ([]byte, error) {
	if packet.ContentHash == "" {
		packet.RefreshHash()
	}
	return json.Marshal(packet)
}

func EncodeBatch(packets []model.Packet) ([]byte, error) {
	return json.Marshal(WireBatch{Packets: packets})
}

func Decode(data []byte) (model.Packet, error) {
	var packet model.Packet
	if err := json.Unmarshal(data, &packet); err != nil {
		return model.Packet{}, errs.New(errs.EPack, "decode packet: %v", err)
	}
	if packet.SampleCount == 0 {
		packet.SampleCount = len(packet.Samples)
	}
	if packet.ContentHash == "" {
		packet.RefreshHash()
	}
	return packet, nil
}

func DecodeBatch(data []byte) ([]model.Packet, error) {
	var batch WireBatch
	if err := json.Unmarshal(data, &batch); err != nil {
		return nil, errs.New(errs.EPack, "decode packet batch: %v", err)
	}
	for i := range batch.Packets {
		if batch.Packets[i].SampleCount == 0 {
			batch.Packets[i].SampleCount = len(batch.Packets[i].Samples)
		}
		if batch.Packets[i].ContentHash == "" {
			batch.Packets[i].RefreshHash()
		}
	}
	return batch.Packets, nil
}

func VerifyOrder(packets []model.Packet) error {
	last := map[int]int{}
	for _, packet := range packets {
		previous, ok := last[packet.Channel]
		if ok && packet.Seq <= previous {
			return &errs.Error{
				Code:      errs.EPack,
				Message:   "packet sequence is not strictly increasing within channel",
				PacketSeq: packet.Seq,
				Channel:   packet.Channel,
				Field:     "seq",
			}
		}
		last[packet.Channel] = packet.Seq
	}
	return nil
}
