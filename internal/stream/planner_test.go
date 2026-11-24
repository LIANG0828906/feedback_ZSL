package stream_test

import (
	"testing"

	"terrain-occlusion/internal/samplegen"
	"terrain-occlusion/internal/stream"
)

func TestMemoryBudgetShortfallQueuesWithoutDropping(t *testing.T) {
	p, err := samplegen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	p.Budgets.LoadSlots = 8
	p.Budgets.MemoryBytes = 1
	p.Budgets.MaxResidencyDelay = 3
	planner := stream.NewPlanner(p.Budgets)
	distances := stream.Distances(p, p.CameraTrack[0].Position)

	result, err := planner.Reconcile(0, p, []int{1, 2}, distances)
	if err != nil {
		t.Fatalf("first observation should only queue, not violate deadline: %v", err)
	}
	if len(result.Loads) != 0 {
		t.Fatalf("tiny memory budget must prevent loads, got %+v", result.Loads)
	}
	if len(result.QueuedIDs) != 2 || result.QueuedIDs[0] != 1 || result.QueuedIDs[1] != 2 {
		t.Fatalf("both chunks must remain queued, got %+v", result.QueuedIDs)
	}
	if planner.Reasons[1] != "memory_budget_exhausted" || planner.Reasons[2] != "memory_budget_exhausted" {
		t.Fatalf("expected memory reasons, got %+v", planner.Reasons)
	}
}

func TestLoadOrderPrefersNearAndCheap(t *testing.T) {
	p, _ := samplegen.Generate()
	p.Budgets.LoadSlots = 1
	p.Budgets.MemoryBytes = 1 << 40
	planner := stream.NewPlanner(p.Budgets)
	distances := map[int]float64{}
	for _, c := range p.Chunks {
		distances[c.ID] = 100
	}
	distances[4] = 1
	distances[9] = 2
	result, err := planner.Reconcile(0, p, []int{9, 4}, distances)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Loads) != 1 || result.Loads[0].ChunkID != 4 {
		t.Fatalf("nearest chunk should win the single slot, got %+v", result.Loads)
	}
	if len(result.QueuedIDs) != 1 || result.QueuedIDs[0] != 9 {
		t.Fatalf("other chunk should queue, got %+v", result.QueuedIDs)
	}
}
