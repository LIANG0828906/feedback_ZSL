package engine

import (
	"fmt"
	"reflect"
	"sort"

	"economycore/model"
)

// CompareRuns finds the first difference between an incremental run and a
// from-empty full run. The returned error is E_DIFF and carries node/window
// context.
func CompareRuns(incremental, full *RunResult) error {
	if incremental == nil || full == nil {
		return model.NewError(model.EDiff, "/runs", "nil run result")
	}
	if len(incremental.Cycles) != len(full.Cycles) {
		return diffError(0, 0, "", "cycles.count", len(incremental.Cycles), len(full.Cycles))
	}
	for index := range incremental.Cycles {
		left := incremental.Cycles[index]
		right := full.Cycles[index]
		if left.Cycle != right.Cycle {
			return diffError(left.Cycle, left.Window, "", "cycle", left.Cycle, right.Cycle)
		}
		if !reflect.DeepEqual(left.CommitSequence, right.CommitSequence) {
			return diffError(left.Cycle, left.Window, "", "commitSequence", left.CommitSequence, right.CommitSequence)
		}
		if err := compareIntMap(left.Cycle, left.Window, "", "globalStocks", left.GlobalStocks, right.GlobalStocks); err != nil {
			return err
		}
		if left.GlobalTotal != right.GlobalTotal {
			return diffError(left.Cycle, left.Window, "", "globalTotal", left.GlobalTotal, right.GlobalTotal)
		}
		if left.WorkersIdle != right.WorkersIdle {
			return diffError(left.Cycle, left.Window, "", "workersIdle", left.WorkersIdle, right.WorkersIdle)
		}
		if len(left.Nodes) != len(right.Nodes) {
			return diffError(left.Cycle, left.Window, "", "nodes.count", len(left.Nodes), len(right.Nodes))
		}
		for nodeIndex := range left.Nodes {
			if err := compareNode(left.Cycle, left.Window, left.Nodes[nodeIndex], right.Nodes[nodeIndex]); err != nil {
				return err
			}
		}
	}
	if err := compareIntMap(0, 0, "", "overflowTotals", incremental.OverflowTotals, full.OverflowTotals); err != nil {
		return err
	}
	if incremental.IntentOffset != full.IntentOffset {
		return diffError(0, 0, "", "intentOffset", incremental.IntentOffset, full.IntentOffset)
	}
	if incremental.Complete != full.Complete {
		return diffError(0, 0, "", "complete", incremental.Complete, full.Complete)
	}
	return nil
}

func compareNode(cycle int64, window int, left, right NodeSnapshot) error {
	if left.NodeID != right.NodeID {
		return diffError(cycle, window, left.NodeID, "nodeId", left.NodeID, right.NodeID)
	}
	nodeID := left.NodeID
	fields := []struct {
		name string
		a    any
		b    any
	}{
		{"built", left.Built, right.Built},
		{"level", left.Level, right.Level},
		{"recipe", left.Recipe, right.Recipe},
		{"workers", left.Workers, right.Workers},
		{"rateFixed", left.RateFixed, right.RateFixed},
		{"stalled", left.Stalled, right.Stalled},
		{"stallStartCycle", left.StallStartCycle, right.StallStartCycle},
	}
	for _, field := range fields {
		if !reflect.DeepEqual(field.a, field.b) {
			return diffError(cycle, window, nodeID, field.name, field.a, field.b)
		}
	}
	for _, mapField := range []struct {
		name string
		a    map[string]int64
		b    map[string]int64
	}{
		{"stocks", left.Stocks, right.Stocks},
		{"produced", left.Produced, right.Produced},
		{"consumed", left.Consumed, right.Consumed},
		{"netRateFixed", left.NetRateFixed, right.NetRateFixed},
		{"overflow", left.Overflow, right.Overflow},
	} {
		if err := compareIntMap(cycle, window, nodeID, mapField.name, mapField.a, mapField.b); err != nil {
			return err
		}
	}
	return nil
}

func compareIntMap(cycle int64, window int, nodeID, field string, left, right map[string]int64) error {
	keys := make(map[string]bool, len(left)+len(right))
	for key := range left {
		keys[key] = true
	}
	for key := range right {
		keys[key] = true
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	for _, key := range sorted {
		a, aok := left[key]
		b, bok := right[key]
		if aok != bok || a != b {
			return diffError(cycle, window, nodeID, field+"."+key, a, b)
		}
	}
	return nil
}

func diffError(cycle int64, window int, nodeID, field string, incremental, full any) error {
	diff := &model.Diff{
		Cycle:       cycle,
		Window:      window,
		NodeID:      nodeID,
		Field:       field,
		Incremental: incremental,
		Full:        full,
	}
	err := model.NewError(model.EDiff, fmt.Sprintf("/cycles/%d/%s", cycle, field), "incremental settlement differs from full settlement")
	err.Diff = diff
	err.NodeID = nodeID
	err.WithWindow(window)
	return err
}
