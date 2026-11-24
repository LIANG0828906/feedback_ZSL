package core

import (
	"fmt"
	"reflect"
	"sort"
)

// CompareTruth returns the first divergence in the requested field order.
func CompareTruth(expected TruthSnapshot, actual Snapshot) *Error {
	err := compareTruth(expected, actual)
	if err != nil && err.Tick == nil {
		err.WithTick(expected.Tick)
	}
	return err
}

func compareTruth(expected TruthSnapshot, actual Snapshot) *Error {
	if expected.Tick != actual.Tick {
		return diverge(expected.Tick, actual.Tick, "", "tick")
	}
	if err := compareNodeSlices(expected.Nodes, actual.Nodes); err != nil {
		return err
	}
	if err := compareMaps("global_stock", expected.GlobalStock, actual.GlobalStock); err != nil {
		return err
	}
	if err := compareMaps("global_net_rate", expected.GlobalNetRate, actual.GlobalNetRate); err != nil {
		return err
	}
	if err := compareMaps("overflow", expected.Overflow, actual.Overflow); err != nil {
		return err
	}
	if err := compareSources(expected.OverflowSources, actual.OverflowSources); err != nil {
		return err
	}
	return nil
}

func CompareSnapshots(expected, actual Snapshot) *Error {
	err := compareSnapshots(expected, actual)
	if err != nil && err.Tick == nil {
		err.WithTick(expected.Tick)
	}
	return err
}

func compareSnapshots(expected, actual Snapshot) *Error {
	if expected.Tick != actual.Tick {
		return diverge(expected.Tick, actual.Tick, "", "tick")
	}
	if expected.IntentOffset != actual.IntentOffset {
		return diverge(expected.IntentOffset, actual.IntentOffset, "", "intent_offset")
	}
	if expected.ParamsVersion != actual.ParamsVersion {
		return diverge(expected.ParamsVersion, actual.ParamsVersion, "", "params_version")
	}
	if expected.SceneHash != actual.SceneHash {
		return diverge(expected.SceneHash, actual.SceneHash, "", "scene_hash")
	}
	if err := compareNodeSlices(expected.Nodes, actual.Nodes); err != nil {
		return err
	}
	if err := compareMaps("global_stock", expected.GlobalStock, actual.GlobalStock); err != nil {
		return err
	}
	if err := compareMaps("global_net_rate", expected.GlobalNetRate, actual.GlobalNetRate); err != nil {
		return err
	}
	if err := compareMaps("overflow", expected.Overflow, actual.Overflow); err != nil {
		return err
	}
	return compareSources(expected.OverflowSources, actual.OverflowSources)
}

func compareNodeSlices(expected, actual []NodeSnapshot) *Error {
	exp := make(map[string]NodeSnapshot, len(expected))
	act := make(map[string]NodeSnapshot, len(actual))
	for _, n := range expected {
		exp[n.ID] = n
	}
	for _, n := range actual {
		act[n.ID] = n
	}
	ids := make([]string, 0, len(exp))
	for id := range exp {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := exp[id]
		a, ok := act[id]
		if !ok {
			return diverge(e, nil, id, "node")
		}
		if e.Type != a.Type {
			return diverge(e.Type, a.Type, id, "type")
		}
		if e.ParentID != a.ParentID {
			return diverge(e.ParentID, a.ParentID, id, "parent_id")
		}
		if e.Level != a.Level {
			return diverge(e.Level, a.Level, id, "level")
		}
		if e.Workers != a.Workers {
			return diverge(e.Workers, a.Workers, id, "workers")
		}
		if e.WorkersRequired != a.WorkersRequired {
			return diverge(e.WorkersRequired, a.WorkersRequired, id, "workers_required")
		}
		if e.Active != a.Active {
			return diverge(e.Active, a.Active, id, "active")
		}
		if e.Stalled != a.Stalled {
			return diverge(e.Stalled, a.Stalled, id, "stalled")
		}
		if !reflect.DeepEqual(e.StallSince, a.StallSince) {
			return diverge(e.StallSince, a.StallSince, id, "stall_since")
		}
		if err := compareMaps("nodes."+id+".stock", e.Stock, a.Stock); err != nil {
			return err
		}
		if err := compareMaps("nodes."+id+".capacity", e.Capacity, a.Capacity); err != nil {
			return err
		}
		if err := compareMaps("nodes."+id+".net_rate", e.NetRate, a.NetRate); err != nil {
			return err
		}
		if err := compareMaps("nodes."+id+".total_output", e.TotalOutput, a.TotalOutput); err != nil {
			return err
		}
		if err := compareMaps("nodes."+id+".total_input", e.TotalInput, a.TotalInput); err != nil {
			return err
		}
	}
	for id := range act {
		if _, ok := exp[id]; !ok {
			return diverge(nil, act[id], id, "node")
		}
	}
	return nil
}

func compareMaps(field string, expected, actual map[string]int64) *Error {
	keys := make(map[string]struct{}, len(expected)+len(actual))
	for k := range expected {
		keys[k] = struct{}{}
	}
	for k := range actual {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		ev := expected[k]
		av := actual[k]
		if ev != av {
			nodeID := ""
			fieldName := field + "." + k
			if len(field) > 6 && field[:6] == "nodes." {
				rest := field[6:]
				for i := 0; i < len(rest); i++ {
					if rest[i] == '.' {
						nodeID = rest[:i]
						break
					}
				}
			}
			e := diverge(ev, av, nodeID, fieldName)
			e.WithField(fieldName)
			return e
		}
	}
	return nil
}

func compareSources(expected, actual []OverflowSource) *Error {
	em := make(map[string]OverflowSource, len(expected))
	am := make(map[string]OverflowSource, len(actual))
	for _, s := range expected {
		em[s.NodeID+"|"+s.Resource] = s
	}
	for _, s := range actual {
		am[s.NodeID+"|"+s.Resource] = s
	}
	keys := make(map[string]struct{}, len(em)+len(am))
	for k := range em {
		keys[k] = struct{}{}
	}
	for k := range am {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		e, eok := em[k]
		a, aok := am[k]
		if !eok || !aok || e != a {
			nodeID := e.NodeID
			if nodeID == "" {
				nodeID = a.NodeID
			}
			return diverge(e, a, nodeID, "overflow_source")
		}
	}
	return nil
}

func diverge(left, right any, nodeID, field string) *Error {
	e := NewError(CodeDiverge, fmt.Sprintf("first divergence at field %s", field)).
		WithValues(left, right)
	e.Field = field
	e.NodeID = nodeID
	return e
}
