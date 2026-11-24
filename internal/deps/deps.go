// Package deps records packet-level dependencies and computes precise
// invalidation after event or capability edits.
package deps

import (
	"fmt"
	"sort"

	"audioforge/internal/model"
)

type ChangeKind string

const (
	ChangeEventPriority    ChangeKind = "event-priority-changed"
	ChangeEventAdded       ChangeKind = "event-added"
	ChangeEventRemoved     ChangeKind = "event-removed"
	ChangeChannelTemplate  ChangeKind = "channel-template-changed"
	ChangeQuantUnit        ChangeKind = "quant-unit-changed"
	ChangeMaxPacketSamples ChangeKind = "max-packet-samples-changed"
)

type Change struct {
	Kind    ChangeKind `json:"kind"`
	EventID string     `json:"event_id,omitempty"`
	Old     any        `json:"old,omitempty"`
	New     any        `json:"new,omitempty"`
}

type Dependency struct {
	PacketSeq      int      `json:"packet_seq"`
	Channel        int      `json:"channel"`
	EventIDs       []string `json:"event_ids"`
	TemplateHash   string   `json:"template_hash"`
	QuantUnit      int64    `json:"quant_unit"`
	PreviousOffset int64    `json:"previous_offset"`
	ContentHash    string   `json:"content_hash"`
}

type Graph struct {
	Packets      []model.Packet `json:"packets"`
	Dependencies []Dependency   `json:"dependencies"`
	byKey        map[string]Dependency
	byPacket     map[string]model.Packet
}

type Invalidated struct {
	PacketSeq int    `json:"packet_seq"`
	Channel   int    `json:"channel"`
	Reason    string `json:"reason"`
}

type Invalidation struct {
	Change       Change         `json:"change"`
	Invalidated  []Invalidated  `json:"invalidated"`
	Reused       []model.Packet `json:"reused_packets"`
	ReusedCount  int            `json:"reused_count"`
	TotalPackets int            `json:"total_packets"`
}

func Build(packets []model.Packet, capability model.Capability) *Graph {
	ordered := append([]model.Packet(nil), packets...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Channel != ordered[j].Channel {
			return ordered[i].Channel < ordered[j].Channel
		}
		return ordered[i].Seq < ordered[j].Seq
	})
	graph := &Graph{
		Packets:  ordered,
		byKey:    make(map[string]Dependency, len(ordered)),
		byPacket: make(map[string]model.Packet, len(ordered)),
	}
	for _, packet := range ordered {
		templateHash := ""
		if packet.Channel >= 0 && packet.Channel < len(capability.Channels) {
			templateHash = model.HashInt64Slice(capability.Channels[packet.Channel].Template)
		}
		dependency := Dependency{
			PacketSeq:      packet.Seq,
			Channel:        packet.Channel,
			EventIDs:       append([]string(nil), packet.DependsOnEvents...),
			TemplateHash:   templateHash,
			QuantUnit:      capability.QuantUnit,
			PreviousOffset: packet.PreviousOffset,
			ContentHash:    packet.ContentHash,
		}
		graph.Dependencies = append(graph.Dependencies, dependency)
		key := packetKey(packet.Channel, packet.Seq)
		graph.byKey[key] = dependency
		graph.byPacket[key] = packet.Clone()
	}
	return graph
}

func (g *Graph) Dependency(channel, seq int) (Dependency, bool) {
	dependency, ok := g.byKey[packetKey(channel, seq)]
	return dependency, ok
}

func (g *Graph) InvalidatedBy(updated *Graph, change Change) Invalidation {
	result := Invalidation{Change: change, TotalPackets: len(updated.Packets)}
	newByChannel := make(map[int]map[int]model.Packet)
	for _, packet := range updated.Packets {
		if newByChannel[packet.Channel] == nil {
			newByChannel[packet.Channel] = make(map[int]model.Packet)
		}
		newByChannel[packet.Channel][packet.Seq] = packet
	}
	oldByChannel := make(map[int]map[int]model.Packet)
	for _, packet := range g.Packets {
		if oldByChannel[packet.Channel] == nil {
			oldByChannel[packet.Channel] = make(map[int]model.Packet)
		}
		oldByChannel[packet.Channel][packet.Seq] = packet
	}
	channels := make([]int, 0, len(oldByChannel)+len(newByChannel))
	seenChannel := make(map[int]bool)
	for channel := range oldByChannel {
		if !seenChannel[channel] {
			channels = append(channels, channel)
			seenChannel[channel] = true
		}
	}
	for channel := range newByChannel {
		if !seenChannel[channel] {
			channels = append(channels, channel)
			seenChannel[channel] = true
		}
	}
	sort.Ints(channels)
	for _, channel := range channels {
		oldPackets := oldByChannel[channel]
		newPackets := newByChannel[channel]
		maxSeq := -1
		for seq := range oldPackets {
			if seq > maxSeq {
				maxSeq = seq
			}
		}
		for seq := range newPackets {
			if seq > maxSeq {
				maxSeq = seq
			}
		}
		firstAffected := -1
		for seq := 0; seq <= maxSeq; seq++ {
			oldPacket, oldOK := oldPackets[seq]
			newPacket, newOK := newPackets[seq]
			if oldOK && newOK && packetSame(oldPacket, newPacket) && g.dependencySame(updated, channel, seq) {
				result.Reused = append(result.Reused, oldPacket.Clone())
				continue
			}
			firstAffected = seq
			break
		}
		if firstAffected < 0 {
			continue
		}
		for seq := firstAffected; seq <= maxSeq; seq++ {
			oldPacket, oldOK := oldPackets[seq]
			newPacket, newOK := newPackets[seq]
			switch {
			case !oldOK && !newOK:
				continue
			case !newOK:
				result.Invalidated = append(result.Invalidated, Invalidated{
					PacketSeq: seq,
					Channel:   channel,
					Reason:    string(change.Kind) + ": packet removed",
				})
			case seq == firstAffected:
				result.Invalidated = append(result.Invalidated, Invalidated{
					PacketSeq: seq,
					Channel:   channel,
					Reason:    string(change.Kind),
				})
			default:
				result.Invalidated = append(result.Invalidated, Invalidated{
					PacketSeq: seq,
					Channel:   channel,
					Reason:    fmt.Sprintf("downstream of invalidated channel %d packet %d", channel, firstAffected),
				})
			}
			_ = oldPacket
			_ = newPacket
		}
	}
	sort.Slice(result.Invalidated, func(i, j int) bool {
		if result.Invalidated[i].Channel != result.Invalidated[j].Channel {
			return result.Invalidated[i].Channel < result.Invalidated[j].Channel
		}
		return result.Invalidated[i].PacketSeq < result.Invalidated[j].PacketSeq
	})
	result.ReusedCount = len(result.Reused)
	return result
}

func (g *Graph) dependencySame(updated *Graph, channel, seq int) bool {
	oldDependency, oldOK := g.Dependency(channel, seq)
	newDependency, newOK := updated.Dependency(channel, seq)
	if oldOK != newOK {
		return false
	}
	if !oldOK {
		return true
	}
	if oldDependency.TemplateHash != newDependency.TemplateHash ||
		oldDependency.QuantUnit != newDependency.QuantUnit ||
		oldDependency.PreviousOffset != newDependency.PreviousOffset ||
		len(oldDependency.EventIDs) != len(newDependency.EventIDs) {
		return false
	}
	for i := range oldDependency.EventIDs {
		if oldDependency.EventIDs[i] != newDependency.EventIDs[i] {
			return false
		}
	}
	return true
}

func packetSame(a, b model.Packet) bool {
	if a.Channel != b.Channel || a.Seq != b.Seq ||
		a.StartSample != b.StartSample || a.SampleCount != b.SampleCount ||
		a.TimingMark != b.TimingMark || a.PreviousOffset != b.PreviousOffset ||
		len(a.Samples) != len(b.Samples) || len(a.DependsOnEvents) != len(b.DependsOnEvents) {
		return false
	}
	for i := range a.Samples {
		if a.Samples[i] != b.Samples[i] {
			return false
		}
	}
	for i := range a.DependsOnEvents {
		if a.DependsOnEvents[i] != b.DependsOnEvents[i] {
			return false
		}
	}
	return a.ContentHash == b.ContentHash
}

func packetKey(channel, seq int) string {
	return fmt.Sprintf("%d/%d", channel, seq)
}
