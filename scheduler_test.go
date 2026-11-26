package loudness

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerBudgetsBlockAndRecordWaits(t *testing.T) {
	config := SchedulerConfig{
		MaxWorkers: 2, QueueCapacity: 1, QueueBudgetSamples: 100,
		BufferBudgetSamples: 100, MaxSamplesPerWorker: 100,
	}
	tasks := make([]ScheduledTask, 0, 6)
	var active atomic.Int64
	var maxActive atomic.Int64
	for i := 0; i < 6; i++ {
		taskID := i
		tasks = append(tasks, ScheduledTask{
			ID: taskID, Label: "clip", SampleCost: 100,
			Run: func(ctx context.Context) (any, error) {
				current := active.Add(1)
				for {
					observed := maxActive.Load()
					if current <= observed || maxActive.CompareAndSwap(observed, current) {
						break
					}
				}
				time.Sleep(12 * time.Millisecond)
				active.Add(-1)
				return taskID, nil
			},
		})
	}
	report, err := RunScheduled(context.Background(), config, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != len(tasks) {
		t.Fatalf("processed %d/%d tasks", len(report.Results), len(tasks))
	}
	if maxActive.Load() > 2 || report.MaxObservedWorkers > 2 {
		t.Fatalf("parallelism exceeded: taskActive=%d reported=%d", maxActive.Load(), report.MaxObservedWorkers)
	}
	reasons := make(map[string]int)
	for _, event := range report.WaitEvents {
		if event.WaitMicros <= 0 || event.TaskID < 0 {
			t.Fatalf("bad wait event: %+v", event)
		}
		reasons[event.Reason]++
	}
	if reasons["sampling-buffer-budget"] == 0 {
		t.Fatalf("no sampling-buffer wait recorded: %+v", report.WaitEvents)
	}
	if reasons["queue-sample-budget"] == 0 && reasons["queue-capacity"] == 0 {
		t.Fatalf("no queue wait recorded: %+v", report.WaitEvents)
	}
}

func TestSchedulerRejectsPerWorkerBudgetOverflow(t *testing.T) {
	config := SchedulerConfig{
		MaxWorkers: 1, QueueCapacity: 1, QueueBudgetSamples: 1000,
		BufferBudgetSamples: 1000, MaxSamplesPerWorker: 100,
	}
	_, err := RunScheduled(context.Background(), config, []ScheduledTask{
		{ID: 1, Label: "too-large", SampleCost: 101, Run: func(context.Context) (any, error) { return nil, nil }},
	})
	if !HasCode(err, EPara) {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	domainErr, _ := AsDomainError(err)
	if domainErr.Field != "sampleCost" || len(domainErr.Operands) != 2 {
		t.Fatalf("budget diagnostics: %+v", domainErr)
	}
}

func TestSchedulerPropagatesTaskError(t *testing.T) {
	config := DefaultSchedulerConfig()
	config.MaxWorkers = 1
	_, err := RunScheduled(context.Background(), config, []ScheduledTask{
		{ID: 1, Label: "ok", SampleCost: 1, Run: func(context.Context) (any, error) { return 1, nil }},
		{ID: 2, Label: "bad", SampleCost: 1, Run: func(context.Context) (any, error) {
			return nil, &DomainError{Code: EPara, Path: "$.task", Detail: "intentional"}
		}},
	})
	if !HasCode(err, EPara) {
		t.Fatalf("task error was not propagated: %v", err)
	}
}
