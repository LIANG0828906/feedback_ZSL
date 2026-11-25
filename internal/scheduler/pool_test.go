package scheduler_test

import (
	"context"
	"testing"
	"time"

	"presentkernel/internal/domain"
	"presentkernel/internal/scheduler"
)

func TestPartitionFailureIsolation(t *testing.T) {
	jobs := []scheduler.Job{
		{Index: 0, Ref: domain.PartitionRef{PartitionID: "P0"}, Run: func(context.Context) (*domain.PartitionResult, error) {
			return nil, domain.StageError("injected E_STAGE")
		}},
		{Index: 1, Ref: domain.PartitionRef{PartitionID: "P1"}, Run: func(context.Context) (*domain.PartitionResult, error) {
			return &domain.PartitionResult{PartitionID: "P1"}, nil
		}},
		{Index: 2, Ref: domain.PartitionRef{PartitionID: "P2"}, Run: func(context.Context) (*domain.PartitionResult, error) {
			return &domain.PartitionResult{PartitionID: "P2"}, nil
		}},
	}
	rep := scheduler.RunJobs(context.Background(), jobs, scheduler.Config{MaxParallel: 2, PerWorkerBuffer: 1, QueueBudget: 2})
	if len(rep.Outcomes) != 3 {
		t.Fatalf("outcomes=%d", len(rep.Outcomes))
	}
	if rep.Outcomes[0].Error == nil || rep.Outcomes[0].Error.Code != domain.CodeStage {
		t.Fatalf("expected E_STAGE, got %+v", rep.Outcomes[0].Error)
	}
	if rep.Outcomes[1].Error != nil || rep.Outcomes[2].Error != nil {
		t.Fatalf("healthy partitions polluted: %+v %+v", rep.Outcomes[1].Error, rep.Outcomes[2].Error)
	}
	if rep.MaxObservedParallel > 2 {
		t.Fatalf("parallelism exceeded budget: %d", rep.MaxObservedParallel)
	}
}

func TestQueueBudgetBackpressure(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		<-started
		time.Sleep(30 * time.Millisecond)
		close(release)
	}()
	jobs := []scheduler.Job{
		{Index: 0, Ref: domain.PartitionRef{PartitionID: "Q0"}, Run: func(context.Context) (*domain.PartitionResult, error) {
			close(started)
			<-release
			return &domain.PartitionResult{PartitionID: "Q0"}, nil
		}},
		budgetOK(1, "Q1"), budgetOK(2, "Q2"),
	}
	rep := scheduler.RunJobs(context.Background(), jobs, scheduler.Config{MaxParallel: 1, PerWorkerBuffer: 1, QueueBudget: 1})
	if !rep.QueueBlocked {
		t.Fatalf("expected queue budget blocking, events=%+v", rep.QueueEvents)
	}
	found := false
	for _, e := range rep.QueueEvents {
		if e.Reason == "queue_budget_exhausted" && e.WaitedMS >= 20 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing measured queue wait: %+v", rep.QueueEvents)
	}
}

func budgetOK(i int, id string) scheduler.Job {
	return scheduler.Job{Index: i, Ref: domain.PartitionRef{PartitionID: id}, Run: func(context.Context) (*domain.PartitionResult, error) {
		return &domain.PartitionResult{PartitionID: id}, nil
	}}
}
