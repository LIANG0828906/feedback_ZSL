package orchestrator

import (
	"reflect"
	"testing"
)

func TestIncrementalDependencyEditMatchesFullRecompute(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewIncrementalEngine(scenario.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	report, err := engine.Apply(Edit{
		Kind:         EditSetDependencies,
		NodeID:       "b",
		Dependencies: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RecomputedTicks) == 0 || len(report.ReusedTicks) == 0 {
		t.Fatalf("expected mixed cache reuse, got %+v", report)
	}
	if !containsInt64(report.ReusedTicks, 0) {
		t.Fatalf("unaffected prefix was not reused: %+v", report)
	}
	assertIncrementalMatchesFull(t, engine)
}

func TestIncrementalZoneEditMatchesFullRecompute(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewIncrementalEngine(scenario.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	report, err := engine.Apply(Edit{Kind: EditSetZone, NodeID: "e", Zone: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.InvalidatedBy) != 1 || report.InvalidatedBy[0] != "zone:e:200->500" {
		t.Fatalf("unexpected invalidation sources: %+v", report)
	}
	assertIncrementalMatchesFull(t, engine)
}

func TestIncrementalAppendedRecoveryExtendsTimeline(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewIncrementalEngine(scenario.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	report, err := engine.Apply(Edit{
		Kind: EditAppendRecovery,
		Event: &Event{
			Tick:   56,
			Seq:    1,
			NodeID: "h",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsInt64(report.RecomputedTicks, 55) || !containsInt64(report.RecomputedTicks, 56) {
		t.Fatalf("timeline extension was not recomputed: %+v", report)
	}
	if !containsInt64(report.ReusedTicks, 54) {
		t.Fatalf("committed prefix was not reused: %+v", report)
	}
	assertIncrementalMatchesFull(t, engine)
}

func TestSnapshotReplacementDropsCache(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewIncrementalEngine(scenario.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := GenerateScenario(DefaultSeed + 1)
	if err != nil {
		t.Fatal(err)
	}
	report, err := engine.ReplaceSnapshot(replacement.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !report.SnapshotChanged || len(report.RecomputedTicks) != len(replacement.Canonical.Events) || len(report.RecomputedTicks) == 0 {
		// The exact length is not the contract; every tick from zero through
		// the final event must be present after a snapshot replacement.
		if !report.SnapshotChanged || len(report.RecomputedTicks) == 0 {
			t.Fatalf("snapshot replacement did not invalidate cache: %+v", report)
		}
	}
	if report.RecomputedTicks[0] != 0 || report.RecomputedTicks[len(report.RecomputedTicks)-1] != 54 {
		t.Fatalf("snapshot replacement recomputed unexpected range: %v", report.RecomputedTicks)
	}
	assertIncrementalMatchesFull(t, engine)

	retained, err := engine.ReplaceSnapshot(replacement.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if retained.SnapshotChanged || len(retained.RecomputedTicks) != 0 {
		t.Fatalf("same snapshot version should retain cache: %+v", retained)
	}
}

func TestConcurrencyAndBackpressureDoNotChangeSemanticResults(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	configs := []SolveOptions{
		{WorkerBudget: 1, PerDomainPendingLimit: 1},
		{WorkerBudget: 3, PerDomainPendingLimit: 2},
		{WorkerBudget: 16, PerDomainPendingLimit: 128},
	}
	var baseline *RunResult
	sawBackpressure := false
	for _, config := range configs {
		solver, err := NewSolver(scenario.Canonical, config)
		if err != nil {
			t.Fatal(err)
		}
		result, err := solver.Solve()
		if err != nil {
			t.Fatal(err)
		}
		for _, tick := range result.Ticks {
			if len(tick.Backpressure) > 0 {
				sawBackpressure = true
			}
		}
		stripVolatile(t, result)
		if baseline == nil {
			baseline = result
		} else if !reflect.DeepEqual(baseline, result) {
			t.Fatalf("worker budget/cap changed semantic results for %+v", config)
		}
	}
	if !sawBackpressure {
		t.Fatal("expected deterministic backpressure under the tightest budget")
	}
}

func TestBudgetNeverExceedsLimit(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	solver, err := NewSolver(scenario.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	result, err := solver.Solve()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tick := range result.Ticks {
		for _, outcome := range tick.Requests {
			if tick.Tick == 8 && outcome.WorkloadID == "wb" && outcome.AmountBytes == 4096 {
				found = true
				if outcome.Status != "denied" || outcome.Reason != "budget_exceeded" {
					t.Fatalf("over-budget request was not denied: %+v", outcome)
				}
			}
			if tick.Tick == 26 && outcome.WorkloadID == "wd" && outcome.AmountBytes == 3072 {
				found = true
				if outcome.Status != "denied" || outcome.Reason != "budget_exceeded" {
					t.Fatalf("over-budget frozen request was not denied on release: %+v", outcome)
				}
			}
		}
	}
	if !found {
		t.Fatal("expected budget boundary requests were not observed")
	}
}

func TestEveryAdmissionStaysWithinBudget(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	solver, err := NewSolver(scenario.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	result, err := solver.Solve()
	if err != nil {
		t.Fatal(err)
	}
	budgets := make(map[string]int64)
	used := make(map[string]int64)
	for _, node := range scenario.Canonical.Nodes {
		for _, workload := range node.Workloads {
			budgets[workload.ID] = workload.BudgetBytes
			used[workload.ID] = workload.UsedBytes
		}
	}
	for _, tick := range result.Ticks {
		for _, outcome := range tick.Requests {
			switch outcome.Status {
			case "admitted", "released_admitted":
				used[outcome.WorkloadID] += outcome.AmountBytes
				if used[outcome.WorkloadID] > budgets[outcome.WorkloadID] {
					t.Fatalf("tick %d admitted %s beyond budget: used=%d budget=%d",
						tick.Tick, outcome.WorkloadID, used[outcome.WorkloadID], budgets[outcome.WorkloadID])
				}
			}
		}
	}
}

func TestNodesNeverOccupyTwoDomains(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	solver, err := NewSolver(scenario.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	result, err := solver.Solve()
	if err != nil {
		t.Fatal(err)
	}
	for _, tick := range result.Ticks {
		seen := make(map[string]int64)
		for _, domain := range tick.Domains {
			for _, nodeID := range domain.Nodes {
				if previous, ok := seen[nodeID]; ok && previous != domain.ID {
					t.Fatalf("tick %d node %s is in domains %d and %d", tick.Tick, nodeID, previous, domain.ID)
				}
				seen[nodeID] = domain.ID
			}
		}
	}
}

func TestExactTimeoutRationalBoundary(t *testing.T) {
	ratio := Ratio{Num: 5, Den: 2}
	if ratio.TimedOutAt(2) {
		t.Fatal("2 ticks must be before a 2.5 tick timeout")
	}
	if !ratio.TimedOutAt(3) {
		t.Fatal("3 ticks must be after a 2.5 tick timeout")
	}
	if !(Ratio{Num: 5, Den: 1}).TimedOutAt(5) {
		t.Fatal("inclusive deadline tick was not detected")
	}
	if (Ratio{Num: 5, Den: 1}).TimedOutAt(4) {
		t.Fatal("pre-deadline tick was reported timed out")
	}
}

func assertIncrementalMatchesFull(t *testing.T, engine *IncrementalEngine) {
	t.Helper()
	incremental, err := engine.RunResult()
	if err != nil {
		t.Fatal(err)
	}
	solver, err := NewSolver(engine.Project())
	if err != nil {
		t.Fatal(err)
	}
	full, err := solver.Solve()
	if err != nil {
		t.Fatal(err)
	}
	stripVolatile(t, incremental)
	stripVolatile(t, full)
	if !reflect.DeepEqual(full, incremental) {
		for index := 0; index < len(full.Ticks) && index < len(incremental.Ticks); index++ {
			if !reflect.DeepEqual(full.Ticks[index], incremental.Ticks[index]) {
				t.Fatalf("incremental differs at tick %d\nfull: %+v\nincremental: %+v", full.Ticks[index].Tick, full.Ticks[index], incremental.Ticks[index])
			}
		}
		t.Fatalf("incremental final differs\nfull: %+v\nincremental: %+v", full.Final, incremental.Final)
	}
}

func stripVolatile(t *testing.T, result *RunResult) {
	t.Helper()
	for index := range result.Ticks {
		result.Ticks[index].InvalidatedBy = nil
		result.Ticks[index].Backpressure = nil
	}
}

func containsInt64(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
