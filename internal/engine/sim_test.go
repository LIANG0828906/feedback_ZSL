package engine_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"warmcache/internal/domain"
	"warmcache/internal/engine"
	"warmcache/internal/gen"
	"warmcache/internal/loader"
)

func loadProject(t *testing.T, version domain.SchemaVersion) domain.Project {
	t.Helper()
	samples, err := gen.Samples(gen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range samples {
		if sample.Version != version {
			continue
		}
		project, err := loader.Load(sample.Raw, version)
		if err != nil {
			t.Fatal(err)
		}
		return project
	}
	t.Fatalf("version %s not generated", version)
	return domain.Project{}
}

func TestScheduleIsEquivalentAcrossSchemas(t *testing.T) {
	var baseline domain.RunResult
	for index, version := range []domain.SchemaVersion{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		project := loadProject(t, version)
		result, err := engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{Ticks: 30, Workers: 3})
		if err != nil {
			t.Fatal(err)
		}
		if got := len(result.Timeline); got != 30 {
			t.Fatalf("%s timeline length %d", version, got)
		}
		if index == 0 {
			baseline = result
			continue
		}
		if err := engine.CompareResults(baseline, result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLeaseBudgetAndGateInvariants(t *testing.T) {
	project := loadProject(t, domain.SchemaV1)
	result, err := engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{Ticks: 30, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, tick := range result.Timeline {
		if tick.UsedBandwidth > tick.Capacity {
			t.Fatalf("tick %d exceeded budget", tick.Tick)
		}
		for _, node := range tick.Nodes {
			if node.Concurrency > 2 {
				t.Fatalf("tick %d node %s exceeded concurrency", tick.Tick, node.NodeID)
			}
		}
	}
	for _, shard := range result.Shards {
		if shard.Generation > 0 && shard.Progress > shard.PreheatTicks {
			t.Fatalf("shard %s progress exceeds target", shard.ShardID)
		}
	}
	codes := map[string]bool{}
	for _, request := range result.Requests {
		if request.Code != "" {
			codes[request.Code] = true
		}
	}
	for _, code := range []string{
		domain.CodeReadFreshUnavailable, domain.CodeWriteVersionConflict, domain.CodeNotFound,
	} {
		if !codes[code] {
			t.Fatalf("missing gate result code %s in %+v", code, result.Requests)
		}
	}
	expired := false
	interrupted := false
	for _, lease := range result.Leases {
		if lease.ReleaseReason == "lease_expired" {
			expired = true
		}
		if lease.ReleaseReason == "interrupted" {
			interrupted = true
		}
	}
	if !expired {
		t.Fatalf("sample did not exercise lease expiry: %+v", result.Leases)
	}
	if interrupted {
		t.Fatal("baseline run should not contain interruption")
	}
	owners := map[string]string{}
	for _, shard := range project.Shards {
		owners[shard.ID] = shard.NodeID
	}
	for _, lease := range result.Leases {
		if lease.Generation == 1 && lease.NodeID != owners[lease.ShardID] {
			t.Fatalf("first lease for %s violated declared node ownership: owner=%s holder=%s", lease.ShardID, owners[lease.ShardID], lease.NodeID)
		}
	}
	firstHolder := map[string]string{}
	reassigned := false
	for _, lease := range result.Leases {
		if lease.Generation == 1 {
			firstHolder[lease.ShardID] = lease.NodeID
		} else if firstHolder[lease.ShardID] != "" && firstHolder[lease.ShardID] != lease.NodeID {
			reassigned = true
		}
	}
	if !reassigned {
		t.Fatal("sample did not demonstrate reassignment to another node after lease loss")
	}
	t.Logf("leases=%d committed=%d expired=%v", len(result.Leases), countAvailable(result.Shards), expired)
}

func TestInterruptResumesAtCommittedStep(t *testing.T) {
	project := loadProject(t, domain.SchemaV2)
	opts := domain.Options{
		Ticks: 30, Workers: 3,
		Interrupts: []domain.Interrupt{{ID: "int-1", Tick: 6, ShardID: "shard-03", Reason: "test", ExitSignal: false}},
	}
	result, err := engine.Run(project, gen.Requests(gen.DefaultSeed), opts)
	if err != nil {
		t.Fatal(err)
	}
	var target domain.ShardState
	for _, shard := range result.Shards {
		if shard.ShardID == "shard-03" {
			target = shard
		}
	}
	if target.CommittedProgress != target.Progress {
		t.Fatalf("uncommitted staging leaked: %+v", target)
	}
	if target.Generation < 2 {
		t.Fatalf("interrupted shard was not re-leased: %+v", target)
	}
	if !target.Available {
		t.Fatalf("interrupted shard did not resume to completion: %+v", target)
	}
	for _, tick := range result.Timeline {
		if tick.Tick != 6 {
			continue
		}
		for _, shard := range tick.Shards {
			if shard.ShardID == "shard-03" {
				if shard.Progress != shard.CommittedProgress {
					t.Fatalf("interruption crossed the committed step boundary: %+v", shard)
				}
				if shard.Available {
					t.Fatalf("interrupted shard became available before full preheat: %+v", shard)
				}
				t.Logf("tick 6 interrupt checkpoint: progress=%d/%d generation=%d", shard.Progress, shard.PreheatTicks, shard.Generation)
			}
		}
	}
}

func TestIncrementalMatchesFullRecompute(t *testing.T) {
	project := loadProject(t, domain.SchemaV3)
	service := engine.NewService(project, gen.Requests(gen.DefaultSeed))
	opts := domain.Options{
		Ticks: 30, Workers: 3,
		Edits: []domain.Edit{
			{ID: "bump-01", Kind: domain.EditBumpShard, AtTick: 7, ShardID: "shard-03"},
			{ID: "retire-01", Kind: domain.EditRetireNode, AtTick: 11, NodeID: "edge-b"},
			{ID: "join-01", Kind: domain.EditJoinNode, AtTick: 12, NodeID: "edge-d", MaxConcurrent: 1, BufferLimit: 1, ClaimIntervalTicks: 2, LeaseTicks: 6},
			{ID: "budget-01", Kind: domain.EditSetBudget, AtTick: 14, BudgetMilli: 150},
		},
	}
	incremental, full, err := service.CompareFull(opts)
	if err != nil {
		t.Fatalf("%v context=%+v", err, domain.AsBizError(err).Context)
	}
	if incremental.ReusedTicks != 6 {
		t.Fatalf("expected six reusable baseline ticks, got %d", incremental.ReusedTicks)
	}
	if !incremental.MatchesFull {
		t.Fatal("comparison flag not set")
	}
	if len(full.Diagnostics.RecomputedTicks) == 0 {
		t.Fatal("full run missing recompute diagnostics")
	}
}

func TestExitSignalStopsAtCommittedStep(t *testing.T) {
	project := loadProject(t, domain.SchemaV1)
	result, err := engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{
		Ticks: 30, Workers: 3,
		Interrupts: []domain.Interrupt{{ID: "exit-1", Tick: 6, ShardID: "shard-03", Reason: "sigterm", ExitSignal: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Interrupted || result.StoppedAtTick != 6 || len(result.Timeline) != 6 {
		t.Fatalf("exit signal did not stop at committed tick: interrupted=%v stopped=%d ticks=%d", result.Interrupted, result.StoppedAtTick, len(result.Timeline))
	}
	for _, shard := range result.Shards {
		if shard.Progress != shard.CommittedProgress {
			t.Fatalf("shard %s has uncommitted progress after exit", shard.ShardID)
		}
	}
}

func TestRetiredShardWriteGateIsClosed(t *testing.T) {
	project := loadProject(t, domain.SchemaV1)
	result, err := engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{
		Ticks: 30, Workers: 3,
		Edits: []domain.Edit{{ID: "retire-shard", Kind: domain.EditRetireShard, AtTick: 4, ShardID: "shard-03"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, request := range result.Requests {
		if request.ShardID == "shard-03" && request.Kind == domain.RequestWrite && request.Code == domain.CodeWriteGateClosed {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected E_WRITE_GATE_CLOSED, requests=%+v", result.Requests)
	}
}

func TestBackpressureAndValidationCodes(t *testing.T) {
	project := loadProject(t, domain.SchemaV1)
	result, err := engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{Ticks: 8, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	foundThread := false
	for _, issue := range result.Backpressure {
		if issue.Code == domain.CodeBackpressureThreads {
			foundThread = true
		}
	}
	if !foundThread {
		t.Fatalf("missing thread backpressure: %+v", result.Backpressure)
	}
	_, err = engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{Ticks: 8, Workers: 300, MaxWorkers: 32})
	if domain.AsBizError(err).Code != domain.CodeThreadBudgetInvalid {
		t.Fatalf("expected thread invalid, got %v", err)
	}
	_, err = engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{Ticks: 8, Workers: 2, MaxBufferPerNode: 999})
	if domain.AsBizError(err).Code != domain.CodeBufferBudgetInvalid {
		t.Fatalf("expected buffer invalid, got %v", err)
	}
}

func TestServiceRunIsDeterministic(t *testing.T) {
	project := loadProject(t, domain.SchemaV1)
	reqs := gen.Requests(gen.DefaultSeed)
	first, err := engine.Run(project, reqs, domain.Options{Ticks: 30, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.Run(project, reqs, domain.Options{Ticks: 30, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first.Timeline)
	secondJSON, _ := json.Marshal(second.Timeline)
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("timeline changed across identical runs")
	}
}

func TestWorkerOrderingDoesNotChangeSchedulingState(t *testing.T) {
	project := loadProject(t, domain.SchemaV1)
	reqs := gen.Requests(gen.DefaultSeed)
	oneWorker, err := engine.Run(project, reqs, domain.Options{Ticks: 30, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	threeWorkers, err := engine.Run(project, reqs, domain.Options{Ticks: 30, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(oneWorker.Timeline) != len(threeWorkers.Timeline) {
		t.Fatal("timeline lengths differ")
	}
	for index := range oneWorker.Timeline {
		left := oneWorker.Timeline[index]
		right := threeWorkers.Timeline[index]
		left.Issues, right.Issues = nil, nil
		leftJSON, _ := json.Marshal(left)
		rightJSON, _ := json.Marshal(right)
		if string(leftJSON) != string(rightJSON) {
			t.Fatalf("tick %d scheduling state depends on worker ordering", left.Tick)
		}
	}
	leftJSON, _ := json.Marshal(oneWorker.Leases)
	rightJSON, _ := json.Marshal(threeWorkers.Leases)
	if string(leftJSON) != string(rightJSON) {
		t.Fatal("lease sequence depends on worker ordering")
	}
}

func countAvailable(shards []domain.ShardState) int {
	count := 0
	for _, shard := range shards {
		if shard.Available {
			count++
		}
	}
	return count
}

func TestDebugSummary(t *testing.T) {
	project := loadProject(t, domain.SchemaV1)
	result, err := engine.Run(project, gen.Requests(gen.DefaultSeed), domain.Options{Ticks: 30, Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for _, tick := range result.Timeline {
		lines = append(lines, fmt.Sprintf("tick=%02d bw=%d queue=%d req=%d", tick.Tick, tick.UsedBandwidth, len(tick.Queue), len(tick.Requests)))
	}
	t.Log("\n" + fmt.Sprint(lines))
}
