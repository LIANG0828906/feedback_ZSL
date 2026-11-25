package deps

import (
	"fmt"
	"sort"

	"lipsync/arbitration"
	"lipsync/domain"
)

type ChangeKind string

const (
	ChangeBudget   ChangeKind = "channel_budget"
	ChangeOrder    ChangeKind = "arbitration_order"
	ChangeRecovery ChangeKind = "recovery_policy"
)

type Change struct {
	Kind      ChangeKind             `json:"kind"`
	ChannelID string                 `json:"channel_id,omitempty"`
	Budget    int64                  `json:"budget_basis_points,omitempty"`
	Order     []string               `json:"order,omitempty"`
	Recovery  *domain.RecoveryPolicy `json:"recovery,omitempty"`
}

type FrameInterval struct {
	ChannelID string   `json:"channel_id"`
	Start     int64    `json:"start_frame"`
	End       int64    `json:"end_frame"`
	Packets   []string `json:"packet_ids"`
}

type InvalidatedFrame struct {
	ChannelID string   `json:"channel_id"`
	Frame     int64    `json:"frame"`
	NodeID    string   `json:"node_id"`
	PacketIDs []string `json:"packet_ids"`
	Reason    string   `json:"reason"`
}

type InvalidationReport struct {
	Change             Change             `json:"change"`
	Code               domain.Code        `json:"code,omitempty"`
	Invalidated        []InvalidatedFrame `json:"invalidated"`
	Intervals          []FrameInterval    `json:"intervals"`
	InvalidatedPackets []string           `json:"invalidated_packet_ids"`
	ReusedFrames       int                `json:"reused_frames"`
	RecomputedFrames   int                `json:"recomputed_frames"`
	StoreHits          int                `json:"store_hits"`
	StoreMisses        int                `json:"store_misses"`
}

// ApplyEdit applies one explicit dependency edit, recomputes the deterministic
// result, and invalidates only frames whose output or recovery schedule changed.
func ApplyEdit(project domain.Project, change Change, store Store) (domain.Project, domain.ArbitrationResult, *Graph, *InvalidationReport, error) {
	oldResult, err := arbitration.Run(project)
	if err != nil {
		return project, domain.ArbitrationResult{}, nil, nil, err
	}
	oldGraph := Build(project, *oldResult)
	if store == nil {
		store = NewMemoryStore()
	}
	for _, id := range oldGraph.NodeOrder {
		node := oldGraph.Nodes[id]
		if node.Kind == NodeFrame && node.Result != nil {
			_ = store.PutFrame(DomainFrameEntry{
				NodeID: node.ID, CacheKey: node.CacheKey, ChannelID: node.ChannelID,
				Frame: node.Frame, Result: *node.Result,
			})
		}
	}
	newProject, err := applyProjectChange(project, change)
	if err != nil {
		return project, domain.ArbitrationResult{}, nil, nil, err
	}
	newResult, err := arbitration.Run(newProject)
	if err != nil {
		return project, domain.ArbitrationResult{}, nil, nil, err
	}
	newGraph := Build(newProject, *newResult)
	report := &InvalidationReport{Change: change}
	oldFrames := frameMap(oldResult.Frames)
	newFrames := frameMap(newResult.Frames)
	keys := make([]string, 0, len(newFrames))
	for key := range newFrames {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	packetChanged := changedPackets(oldResult.PacketStates, newResult.PacketStates)
	for _, key := range keys {
		newFrame := newFrames[key]
		newChannel := newFrame.Result
		oldFrame, exists := oldFrames[key]
		oldChannel := oldFrame.Result
		nodeID := frameNodeID(newChannel.ChannelID, newFrame.Frame)
		newNode, ok := newGraph.Nodes[nodeID]
		if !ok || newNode.Result == nil {
			continue
		}
		changed := change.Kind == ChangeOrder || !exists || !sameFrameChannel(oldChannel, newChannel)
		if !changed && packetTouchesFrame(packetChanged, oldChannel) {
			changed = true
		}
		if !changed {
			entry, hit := store.GetFrame(nodeID)
			if hit {
				// The semantic output is unchanged; revalidate the persisted
				// payload instead of recomputing its downstream event payload.
				replaceFrameChannel(newResult, newChannel.ChannelID, newFrame.Frame, entry.Result)
				report.ReusedFrames++
				_ = store.PutFrame(DomainFrameEntry{
					NodeID: nodeID, CacheKey: newNode.CacheKey, ChannelID: newNode.ChannelID,
					Frame: newNode.Frame, Result: entry.Result,
				})
				continue
			}
			report.StoreMisses++
		}
		_ = store.DeleteFrame(nodeID)
		packetIDs := framePacketIDs(newChannel)
		report.Invalidated = append(report.Invalidated, InvalidatedFrame{
			ChannelID: newChannel.ChannelID, Frame: newFrame.Frame, NodeID: nodeID,
			PacketIDs: packetIDs, Reason: invalidationReason(change, oldChannel, newChannel),
		})
		report.RecomputedFrames++
		for _, id := range packetIDs {
			report.InvalidatedPackets = append(report.InvalidatedPackets, id)
		}
		payload := *newNode.Result
		_ = store.PutFrame(DomainFrameEntry{
			NodeID: nodeID, CacheKey: newNode.CacheKey, ChannelID: newNode.ChannelID,
			Frame: newNode.Frame, Result: payload,
		})
	}
	for _, state := range newResult.PacketStates {
		if _, changed := packetChanged[state.ID]; changed {
			report.InvalidatedPackets = append(report.InvalidatedPackets, state.ID)
		}
	}
	report.InvalidatedPackets = uniqueSorted(report.InvalidatedPackets)
	report.Intervals = buildIntervals(report.Invalidated)
	report.StoreHits = report.ReusedFrames
	newGraph.Result = *newResult
	return newProject, *newResult, newGraph, report, nil
}

func applyProjectChange(project domain.Project, change Change) (domain.Project, error) {
	updated := project
	updated.Channels = append([]domain.Channel(nil), project.Channels...)
	updated.ArbitrationOrder = append([]string(nil), project.ArbitrationOrder...)
	switch change.Kind {
	case ChangeBudget:
		found := false
		for i := range updated.Channels {
			if updated.Channels[i].ID == change.ChannelID {
				updated.Channels[i].FrameBudgetBasisPoints = change.Budget
				found = true
			}
		}
		if !found {
			return project, &domain.Error{Code: domain.CodeSchema, Path: "change.channel_id", ChannelID: change.ChannelID, Message: "channel not found"}
		}
	case ChangeOrder:
		if len(change.Order) != len(updated.Channels) {
			return project, &domain.Error{Code: domain.CodeSchema, Path: "change.order", Message: "order must cover all channels"}
		}
		updated.ArbitrationOrder = append([]string(nil), change.Order...)
	case ChangeRecovery:
		if change.Recovery == nil {
			return project, &domain.Error{Code: domain.CodeSchema, Path: "change.recovery", Message: "recovery policy is required"}
		}
		updated.Recovery = *change.Recovery
	default:
		return project, &domain.Error{Code: domain.CodeSchema, Path: "change.kind", Message: "unknown edit kind"}
	}
	updated.ContentHash = domain.ComputeContentHash(updated)
	return updated, nil
}

type frameChannelAt struct {
	Frame  int64
	Result domain.FrameChannelResult
}

func frameMap(frames []domain.FrameResult) map[string]frameChannelAt {
	out := map[string]frameChannelAt{}
	for _, frame := range frames {
		for _, channel := range frame.Channels {
			out[frameKey(channel.ChannelID, frame.Frame)] = frameChannelAt{Frame: frame.Frame, Result: channel}
		}
	}
	return out
}

func frameKey(channelID string, frame int64) string {
	return fmt.Sprintf("%s\x00%d", channelID, frame)
}

func sameFrameChannel(a, b domain.FrameChannelResult) bool {
	if a.ChannelID != b.ChannelID || a.OccupiedSlot != b.OccupiedSlot || a.UsedBudget != b.UsedBudget {
		return false
	}
	return sameStrings(a.Accepted, b.Accepted) && sameStrings(a.Delayed, b.Delayed) &&
		sameStrings(a.Preempted, b.Preempted) && sameStrings(a.Resumed, b.Resumed)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func framePacketIDs(result domain.FrameChannelResult) []string {
	return uniqueSorted(append(append(append(append([]string{},
		result.Accepted...), result.Delayed...), result.Preempted...), result.Resumed...))
}

func uniqueSorted(in []string) []string {
	out := uniqueStrings(in)
	sort.Strings(out)
	return out
}

func changedPackets(old, updated []domain.PacketState) map[string]bool {
	oldByID := map[string]domain.PacketState{}
	for _, state := range old {
		oldByID[state.ID] = state
	}
	changed := map[string]bool{}
	for _, state := range updated {
		previous, ok := oldByID[state.ID]
		if !ok || previous.RecoveryFrame != state.RecoveryFrame ||
			previous.PreemptedAt != state.PreemptedAt ||
			previous.Delivered != state.Delivered ||
			previous.AbandonedAt != state.AbandonedAt ||
			previous.Status != state.Status {
			changed[state.ID] = true
		}
	}
	return changed
}

func packetTouchesFrame(changed map[string]bool, frame domain.FrameChannelResult) bool {
	for _, id := range framePacketIDs(frame) {
		if changed[id] {
			return true
		}
	}
	return false
}

func replaceFrameChannel(result *domain.ArbitrationResult, channelID string, frame int64, replacement domain.FrameChannelResult) {
	for i := range result.Frames {
		if result.Frames[i].Frame != frame {
			continue
		}
		for j := range result.Frames[i].Channels {
			if result.Frames[i].Channels[j].ChannelID == channelID {
				result.Frames[i].Channels[j] = replacement
				return
			}
		}
	}
}

func invalidationReason(change Change, old, updated domain.FrameChannelResult) string {
	switch change.Kind {
	case ChangeOrder:
		return "arbitration_order changed; aggregate frame channel order changed"
	case ChangeBudget:
		return "channel budget changed frame allocation"
	case ChangeRecovery:
		return "recovery policy changed resume/preemption schedule"
	default:
		return "frame result changed"
	}
}

func buildIntervals(frames []InvalidatedFrame) []FrameInterval {
	var intervals []FrameInterval
	type key struct {
		channel string
	}
	byChannel := map[key][]InvalidatedFrame{}
	for _, frame := range frames {
		k := key{channel: frame.ChannelID}
		byChannel[k] = append(byChannel[k], frame)
	}
	channelNames := make([]string, 0, len(byChannel))
	for k := range byChannel {
		channelNames = append(channelNames, k.channel)
	}
	sort.Strings(channelNames)
	for _, channel := range channelNames {
		list := byChannel[key{channel: channel}]
		sort.Slice(list, func(i, j int) bool { return list[i].Frame < list[j].Frame })
		current := FrameInterval{ChannelID: channel, Start: list[0].Frame, End: list[0].Frame}
		packets := append([]string(nil), list[0].PacketIDs...)
		for _, frame := range list[1:] {
			if frame.Frame != current.End+1 {
				current.Packets = uniqueSorted(packets)
				intervals = append(intervals, current)
				current = FrameInterval{ChannelID: channel, Start: frame.Frame, End: frame.Frame}
				packets = append([]string(nil), frame.PacketIDs...)
				continue
			}
			current.End = frame.Frame
			packets = append(packets, frame.PacketIDs...)
		}
		current.Packets = uniqueSorted(packets)
		intervals = append(intervals, current)
	}
	return intervals
}
