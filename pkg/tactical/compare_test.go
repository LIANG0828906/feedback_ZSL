package tactical_test

import (
	"context"
	"encoding/json"
	"testing"

	"tacticalworkbench/pkg/tactical"
)

func TestCompareSnapshotsLocatesFirstBusinessMismatch(t *testing.T) {
	scenario := baseScenario(t)
	engine := newEngine(t, scenario)
	snapshot, err := engine.FullRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mutated := cloneSnapshotForTest(t, snapshot)
	mutated.Paths["A-REAR"].Cells[3] = tactical.GridPoint{X: 99, Y: 99}
	err = tactical.CompareSnapshots(snapshot, mutated)
	requireCodeField(t, err, tactical.CodeDiff, "")
	typed, _ := tactical.AsError(err)
	if typed.MemberID != "A-REAR" || typed.Item != "path.cells[3]" {
		t.Fatalf("unexpected first path diff: %v", err)
	}

	mutated = cloneSnapshotForTest(t, snapshot)
	trace := mutated.Traces["A-LEAD"]
	trace.States[7].Position.X += 1000
	mutated.Traces["A-LEAD"] = trace
	err = tactical.CompareSnapshots(snapshot, mutated)
	requireCodeField(t, err, tactical.CodeDiff, "")
	typed, _ = tactical.AsError(err)
	if typed.MemberID != "A-LEAD" || typed.Tick != 7 || typed.Item != "trace.position" {
		t.Fatalf("unexpected first trace diff: %v", err)
	}
}

func cloneSnapshotForTest(t *testing.T, snapshot *tactical.Snapshot) *tactical.Snapshot {
	t.Helper()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var out tactical.Snapshot
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}
