package packetize

import "sort"

// EditSpec 描述一次编辑（事件优先级修改/增删事件，或量化参数调整）。
type EditSpec struct {
	EditedEventIDs    []string // 被编辑或删除的事件 ID；新增事件给一个新 ID 并填 TouchedChannels
	AddedEventIDs     []string
	TouchedChannels   []int
	QuantUnitChanged  bool
	PacketSizeChanged bool
}

// InvalidationEntry 是失效/复用清单中的一条，顺序与包提交顺序稳定对应。
type InvalidationEntry struct {
	Seq     int    `json:"seq"`
	Channel int    `json:"channel"`
	Reason  string `json:"reason"`
	Reused  bool   `json:"reused"`
}

// BuildDependencyIndex 建立包序号到依赖事件集合的索引（显式依赖图）。
func BuildDependencyIndex(packets []*Packet) map[int]*Packet {
	idx := map[int]*Packet{}
	for _, pkt := range packets {
		idx[pkt.Seq] = pkt
	}
	return idx
}

// PropagateInvalidation 仅把受影响包及其后续同通道包标记失效，其余标记复用缓存。
func PropagateInvalidation(packets []*Packet, edit EditSpec) []InvalidationEntry {
	// 全局参数变化会影响所有通道。
	globalChange := edit.QuantUnitChanged || edit.PacketSizeChanged
	touched := map[int]bool{}
	for _, c := range edit.TouchedChannels {
		touched[c] = true
	}
	edited := map[string]bool{}
	for _, id := range append(append([]string{}, edit.EditedEventIDs...), edit.AddedEventIDs...) {
		edited[id] = true
	}

	// 每通道第一个失效包的通道内序号。
	firstInvalid := map[int]int{}
	for _, pkt := range packets {
		if globalChange || touched[pkt.Channel] {
			if _, ok := firstInvalid[pkt.Channel]; !ok {
				firstInvalid[pkt.Channel] = pkt.ChannelIndex
			}
			continue
		}
		for _, dep := range pkt.DependEvents {
			if edited[dep] {
				if _, ok := firstInvalid[pkt.Channel]; !ok {
					firstInvalid[pkt.Channel] = pkt.ChannelIndex
				}
				break
			}
		}
	}

	entries := make([]InvalidationEntry, 0, len(packets))
	for _, pkt := range packets {
		start, hit := firstInvalid[pkt.Channel]
		invalid := hit && pkt.ChannelIndex >= start
		entry := InvalidationEntry{Seq: pkt.Seq, Channel: pkt.Channel}
		switch {
		case globalChange && invalid:
			entry.Reason = "quant-params-changed"
		case invalid && dependsOn(pkt, edited):
			entry.Reason = "event-edit"
		case invalid:
			entry.Reason = "downstream-same-channel"
		default:
			entry.Reason = "cached"
			entry.Reused = true
		}
		entries = append(entries, entry)
	}
	// 稳定排序：与包提交顺序（Seq）一致。
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
	return entries
}

func dependsOn(pkt *Packet, edited map[string]bool) bool {
	for _, dep := range pkt.DependEvents {
		if edited[dep] {
			return true
		}
	}
	return false
}
