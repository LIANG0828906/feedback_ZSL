package hckernel

import (
	"context"
	"testing"
)

func tightProject(priority SlotPriority) *Project {
	return &Project{
		ProjectID: "tight", ProjectVersion: "test", StepMs: 100, Rounds: 2, MaxDeferMs: 1000,
		GlobalConcurrency: 1,
		Racks:             []Rack{{ID: "r", Concurrency: 2}},
		Workers:           2, BufferCapacity: 4, SlotPriority: priority,
		CountPolicy: CountPolicy{
			SuccessReachedAction: ThresholdReset, FailureReachedAction: ThresholdReset,
			ResetOppositeStreak: true, HalfOpenSuccessLimit: 1, HalfOpenFailureLimit: 1, HalfOpenDelayMs: 100,
		},
		Instances: []Instance{
			{ID: "a", RackID: "r", Protocol: "HTTP", IntervalMs: 100, TimeoutMs: 50, BackoffMs: 0, SuccessThreshold: 1, FailureThreshold: 5, InitialState: StateHealthy, WeightPPM: 900000, ActiveProbe: true},
			{ID: "b", RackID: "r", Protocol: "HTTP", IntervalMs: 100, TimeoutMs: 50, BackoffMs: 0, SuccessThreshold: 1, FailureThreshold: 5, InitialState: StateHealthy, WeightPPM: 100000, ActiveProbe: true},
		},
	}
}

func TestZeroBudgetRejectsInsteadOfDispatching(t *testing.T) {
	p := tightProject(PriorityDeferredFirst)
	p.GlobalConcurrency = 0
	e, err := NewEngine(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Advance(0, 1); err != nil {
		t.Fatal(err)
	}
	r := e.reports[0]
	if len(r.Dispatched) != 0 {
		t.Fatalf("zero budget dispatched %v", r.Dispatched)
	}
	if len(r.Rejected) != 2 || !IsCode(&Error{Code: r.Rejected[0].Code}, ErrBudgetZero) {
		t.Fatalf("expected E_BUDGET_ZERO rejections, got %+v", r.Rejected)
	}
}

func TestSlotPriorityIsDeterministic(t *testing.T) {
	deferredFirst := tightProject(PriorityDeferredFirst)
	e1, _ := NewEngine(deferredFirst, nil)
	_ = e1.Advance(0, 1) // a gets only global slot; b is queued.
	_ = e1.Advance(100, 2)
	if got := e1.reports[1].Dispatched; len(got) != 1 || got[0].InstanceID != "b" {
		t.Fatalf("deferred_first round 2 = %+v, want b", got)
	}

	periodicFirst := tightProject(PriorityPeriodicFirst)
	e2, _ := NewEngine(periodicFirst, nil)
	_ = e2.Advance(0, 1)
	// At t100 both a's new period and queued b compete for one slot.
	_ = e2.Advance(100, 2)
	if got := e2.reports[1].Dispatched; len(got) != 1 || got[0].InstanceID != "a" {
		t.Fatalf("periodic_first round 2 = %+v, want a", got)
	}

	// Repeated runs and shuffled submission order must not alter plan.
	p, events := sampleProjectEvents(t)
	res, err := RunParallel(context.Background(), p, events, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Batches) != p.Rounds {
		t.Fatalf("batches=%d want %d", len(res.Batches), p.Rounds)
	}
	for round := range res.Batches {
		planned := res.Result.Rounds[round].Dispatched
		shuffled := ShuffleForTest(planned, round+7)
		exec, _ := NewParallelExecutor(4, p.BufferCapacity)
		out, err := exec.ExecuteBatch(context.Background(), round+1, shuffled, (round%4)+1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Outputs) != len(planned) {
			t.Fatalf("round %d outputs=%d planned=%d", round+1, len(out.Outputs), len(planned))
		}
	}
}
