package core_test

import (
	"testing"

	"economycore/internal/core"
)

func TestCompareLocatesFirstDivergence(t *testing.T) {
	s := baseScenario(t)
	run, err := core.RunFull(s, []int64{4, 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Snapshots) < 2 {
		t.Fatal("expected snapshots")
	}
	left := run.Snapshots[0]
	right := run.Snapshots[0]
	right.Nodes = append([]core.NodeSnapshot(nil), right.Nodes...)
	for i := range right.Nodes {
		if right.Nodes[i].ID == "factory" {
			right.Nodes[i].Stock = copyIntMap(right.Nodes[i].Stock)
			right.Nodes[i].Stock["food"]++
			break
		}
	}
	diff := core.CompareSnapshots(left, right)
	if core.CodeOf(diff) != core.CodeDiverge {
		t.Fatalf("code=%s want E_DIVERGE: %v", core.CodeOf(diff), diff)
	}
	if diff.Tick == nil || *diff.Tick != left.Tick {
		t.Fatalf("tick missing: %+v", diff.Tick)
	}
	if diff.NodeID != "factory" || diff.Field != "nodes.factory.stock.food" {
		t.Fatalf("bad locator: node=%q field=%q", diff.NodeID, diff.Field)
	}
	if diff.Left == nil || diff.Right == nil {
		t.Fatalf("both sides must be present: %+v", diff)
	}
}

func copyIntMap(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
