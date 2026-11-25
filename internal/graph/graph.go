package graph

import (
	"fmt"
	"sort"

	"audiobus/domain"
)

type Cache struct {
	Packets map[Key]domain.Packet
}

type Key struct {
	Window  int
	Channel int
	Seq     int
}

func NewCache() *Cache {
	return &Cache{Packets: map[Key]domain.Packet{}}
}

func (c *Cache) Get(p domain.Packet) (domain.Packet, bool) {
	v, ok := c.Packets[Key{p.Window, p.Channel, p.Seq}]
	return v, ok
}

func (c *Cache) Put(p domain.Packet) {
	c.Packets[Key{p.Window, p.Channel, p.Seq}] = p
}

func (c *Cache) Apply(invalidations []domain.Invalidation) {
	for _, inv := range invalidations {
		delete(c.Packets, Key{inv.Window, inv.Channel, inv.Seq})
	}
}

func EventEdit(packets []domain.Packet, eventIDs ...int64) []domain.Invalidation {
	idSet := map[int64]bool{}
	for _, id := range eventIDs {
		idSet[id] = true
	}
	byChannel := group(packets)
	out := []domain.Invalidation{}
	for ch, list := range byChannel {
		first := -1
		for i, p := range list {
			for _, id := range p.EventIDs {
				if idSet[id] {
					first = i
					break
				}
			}
			if first >= 0 {
				break
			}
		}
		if first < 0 {
			continue
		}
		for i := first; i < len(list); i++ {
			p := list[i]
			out = append(out, domain.Invalidation{Window: p.Window, Channel: ch, Seq: p.Seq, Reason: fmt.Sprintf("event %v dependency or downstream order changed", eventIDs)})
		}
	}
	sortInvalidations(out)
	return out
}

func AddedEvent(packets []domain.Packet, channel int, start int) []domain.Invalidation {
	byChannel := group(packets)
	list := byChannel[channel]
	first := -1
	for i, p := range list {
		if start >= p.Start && start < p.Start+p.CapacitySamples {
			first = i
			break
		}
		if p.Start >= start {
			first = i
			break
		}
	}
	if first < 0 {
		return nil
	}
	out := []domain.Invalidation{}
	for i := first; i < len(list); i++ {
		p := list[i]
		out = append(out, domain.Invalidation{Window: p.Window, Channel: channel, Seq: p.Seq, Reason: "event added in or before packet"})
	}
	return out
}

func QuantUnitChange(packets []domain.Packet, oldUnit, newUnit int64) []domain.Invalidation {
	if oldUnit == newUnit {
		return nil
	}
	byChannel := group(packets)
	out := []domain.Invalidation{}
	for ch, list := range byChannel {
		first := -1
		for i, p := range list {
			if p.QuantUnit != newUnit && !allZero(p.Frames) {
				first = i
				break
			}
		}
		if first < 0 {
			continue
		}
		for i := first; i < len(list); i++ {
			p := list[i]
			out = append(out, domain.Invalidation{Window: p.Window, Channel: ch, Seq: p.Seq, Reason: fmt.Sprintf("quantUnit %d -> %d", oldUnit, newUnit)})
		}
	}
	sortInvalidations(out)
	return out
}

func PacketCapChange(packets []domain.Packet, oldCap, newCap int) []domain.Invalidation {
	if oldCap == newCap {
		return nil
	}
	out := make([]domain.Invalidation, 0, len(packets))
	for _, p := range packets {
		out = append(out, domain.Invalidation{Window: p.Window, Channel: p.Channel, Seq: p.Seq, Reason: fmt.Sprintf("maxSamplesPerPack %d -> %d changes boundaries", oldCap, newCap)})
	}
	sortInvalidations(out)
	return out
}

func group(packets []domain.Packet) map[int][]domain.Packet {
	out := map[int][]domain.Packet{}
	for _, p := range packets {
		out[p.Channel] = append(out[p.Channel], p)
	}
	for ch := range out {
		sort.SliceStable(out[ch], func(i, j int) bool {
			if out[ch][i].Window != out[ch][j].Window {
				return out[ch][i].Window < out[ch][j].Window
			}
			return out[ch][i].Seq < out[ch][j].Seq
		})
	}
	return out
}

func allZero(v []int64) bool {
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

func sortInvalidations(in []domain.Invalidation) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Channel != in[j].Channel {
			return in[i].Channel < in[j].Channel
		}
		if in[i].Window != in[j].Window {
			return in[i].Window < in[j].Window
		}
		return in[i].Seq < in[j].Seq
	})
}
