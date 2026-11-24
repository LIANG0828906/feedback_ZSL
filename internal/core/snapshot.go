package core

import "sort"

func (e *Engine) Snapshot() Snapshot {
	state := e.state
	nodeIDs := make([]string, 0, len(state.Nodes))
	for id := range state.Nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	snap := Snapshot{
		Tick:             state.Tick,
		IntentOffset:     state.IntentOffset,
		Nodes:            make([]NodeSnapshot, 0, len(nodeIDs)),
		GlobalStock:      make(map[string]int64),
		GlobalNetRate:    make(map[string]int64),
		Overflow:         cloneMap(state.Overflow),
		OverflowSources:  state.OverflowSources(),
		ParamsVersion:    e.scene.ParamsVersion,
		SceneHash:        e.scene.SceneHash,
		AlgorithmVersion: AlgorithmVersion,
	}
	for _, id := range nodeIDs {
		n := state.Nodes[id]
		ns := NodeSnapshot{
			ID:              n.ID,
			Type:            n.Type,
			ParentID:        n.ParentID,
			Level:           n.Level,
			Workers:         n.Workers,
			WorkersRequired: n.Params.WorkersRequired,
			Active:          n.Active,
			Stalled:         n.Stalled,
			Stock:           cloneMap(n.Stock),
			Capacity:        cloneMap(n.Params.Capacity),
			NetRate:         cloneMap(n.NetRate),
			TotalOutput:     cloneMap(n.TotalOutput),
			TotalInput:      cloneMap(n.TotalInput),
		}
		if n.StallSince != nil {
			t := *n.StallSince
			ns.StallSince = &t
		}
		snap.Nodes = append(snap.Nodes, ns)
		for r, v := range n.Stock {
			snap.GlobalStock[r] += v
		}
		for r, v := range n.NetRate {
			snap.GlobalNetRate[r] += v
		}
	}
	return snap
}

func (s *State) OverflowSources() []OverflowSource {
	out := make([]OverflowSource, 0, len(s.Overflow))
	for key, total := range s.Overflow {
		nodeID, resource, ok := splitOverflowKey(key)
		if !ok {
			continue
		}
		out = append(out, OverflowSource{
			NodeID:   nodeID,
			Resource: resource,
			Total:    total,
			LastTick: s.OverflowLast[key],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID == out[j].NodeID {
			return out[i].Resource < out[j].Resource
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

func TruthFromSnapshot(s Snapshot) TruthSnapshot {
	return TruthSnapshot{
		Tick:            s.Tick,
		Nodes:           s.Nodes,
		GlobalStock:     cloneMap(s.GlobalStock),
		GlobalNetRate:   cloneMap(s.GlobalNetRate),
		Overflow:        cloneMap(s.Overflow),
		OverflowSources: append([]OverflowSource(nil), s.OverflowSources...),
	}
}
