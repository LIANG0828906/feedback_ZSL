package hckernel

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

func TestThresholdActionsResetAndKeep(t *testing.T) {
	p := &Project{
		CountPolicy: CountPolicy{
			SuccessReachedAction: ThresholdReset,
			FailureReachedAction: ThresholdKeep,
			ResetOppositeStreak:  true,
		},
	}
	e := &Engine{project: *p}
	rt := &InstanceState{
		Instance: Instance{ID: "i", SuccessThreshold: 2, FailureThreshold: 2},
		Health:   StateInitial,
	}

	e.applyOutcome(rt, ResultSuccess, "E1", 0, "p1")
	if rt.SuccessStreak != 1 {
		t.Fatalf("success streak=%d want 1", rt.SuccessStreak)
	}
	e.applyOutcome(rt, ResultSuccess, "E2", 10, "p2")
	if rt.Health != StateHealthy || rt.SuccessStreak != 0 {
		t.Fatalf("after success threshold health=%s streak=%d", rt.Health, rt.SuccessStreak)
	}

	e.applyOutcome(rt, ResultFailure, "E3", 20, "p3")
	e.applyOutcome(rt, ResultFailure, "E4", 30, "p4")
	if rt.Health != StateUnhealthy || rt.FailureStreak != 2 {
		t.Fatalf("keep policy health=%s failure streak=%d want 2", rt.Health, rt.FailureStreak)
	}
}

func TestParallelShuffledBatchPreservesProbeIDSet(t *testing.T) {
	p, events := sampleProjectEvents(t)
	result, err := NewEngine(p, events)
	if err != nil {
		t.Fatal(err)
	}
	full, err := result.Run()
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewParallelExecutor(4, p.BufferCapacity)
	if err != nil {
		t.Fatal(err)
	}
	for roundIndex, round := range full.Rounds {
		plannedIDs := make([]string, 0, len(round.Dispatched))
		for _, probe := range round.Dispatched {
			plannedIDs = append(plannedIDs, probe.ID)
		}
		shuffled := ShuffleForTest(round.Dispatched, roundIndex*17+3)
		batch, err := executor.ExecuteBatch(context.Background(), round.Round, shuffled, (roundIndex%4)+1, nil)
		if err != nil {
			t.Fatalf("round %d: %v", round.Round, err)
		}
		gotIDs := make([]string, 0, len(batch.Outputs))
		for _, out := range batch.Outputs {
			gotIDs = append(gotIDs, out.ProbeID)
		}
		sort.Strings(plannedIDs)
		sort.Strings(gotIDs)
		if !reflect.DeepEqual(plannedIDs, gotIDs) {
			t.Fatalf("round %d IDs differ planned=%v got=%v", round.Round, plannedIDs, gotIDs)
		}
	}
}

func TestShuffledDeclarationOrderSameResult(t *testing.T) {
	g := NewGenerator(DefaultSeed)
	p1 := g.BuildProject()
	events := g.BuildEvents()
	p2 := CloneProject(*p1)
	// Reverse declaration order; canonical scheduling sorts IDs independently.
	for i, j := 0, len(p2.Instances)-1; i < j; i, j = i+1, j-1 {
		p2.Instances[i], p2.Instances[j] = p2.Instances[j], p2.Instances[i]
	}
	r1, err := NewEngine(p1, events)
	if err != nil {
		t.Fatal(err)
	}
	a, err := r1.Run()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := NewEngine(&p2, events)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r2.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !ResultsEqualSemantic(a, b) {
		t.Fatal("reversed instance declaration order changed scheduling result")
	}
}
