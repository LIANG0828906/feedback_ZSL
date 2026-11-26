package mirror

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunTasksHonorsBudgetAndRecordsBackpressure(t *testing.T) {
	var ran int64
	tasks := make([]Task[int], 24)
	for i := range tasks {
		value := i
		tasks[i] = Task[int]{
			ID: "task",
			Run: func(context.Context) (int, error) {
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt64(&ran, 1)
				return value, nil
			},
		}
	}
	results, errs, stats, err := RunTasks(context.Background(), tasks, BudgetConfig{
		MaxConcurrency: 2, BufferPerWorker: 1, QueueBudget: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&ran); got != int64(len(tasks)) {
		t.Fatalf("ran %d tasks, want %d", got, len(tasks))
	}
	for i, v := range results {
		if v != i {
			t.Fatalf("result[%d] = %d", i, v)
		}
		if errs[i] != nil {
			t.Fatalf("task %d error: %v", i, errs[i])
		}
	}
	if stats.MaxObservedWorkers > 2 || stats.MaxObservedWorkers < 1 {
		t.Fatalf("max workers = %d", stats.MaxObservedWorkers)
	}
	if len(stats.QueueRecords) == 0 {
		t.Fatal("expected recorded queue backpressure")
	}
	for _, record := range stats.QueueRecords {
		if record.Reason == "" || record.TaskID == "" {
			t.Fatalf("incomplete queue record: %+v", record)
		}
	}
}

func TestRunTasksIsolatesErrors(t *testing.T) {
	tasks := []Task[int]{
		{ID: "ok", Run: func(context.Context) (int, error) { return 1, nil }},
		{ID: "bad", Run: func(context.Context) (int, error) { return 0, &Error{Code: CodeCapacity, ScreenID: "s"} }},
		{ID: "ok2", Run: func(context.Context) (int, error) { return 3, nil }},
	}
	results, errs, _, err := RunTasks(context.Background(), tasks, BudgetConfig{
		MaxConcurrency: 2, BufferPerWorker: 1, QueueBudget: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0] != 1 || results[2] != 3 || errs[0] != nil || errs[2] != nil {
		t.Fatalf("unexpected results=%v errs=%v", results, errs)
	}
	if !IsCode(errs[1], CodeCapacity) {
		t.Fatalf("isolated task error = %v", errs[1])
	}
}

func TestRunTasksRejectsInvalidBudget(t *testing.T) {
	_, _, _, err := RunTasks[int](context.Background(), nil, BudgetConfig{})
	if !IsCode(err, CodeState) {
		t.Fatalf("error = %v, want E_STATE", err)
	}
}
