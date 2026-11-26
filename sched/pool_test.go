package sched_test

import (
	"context"
	"testing"
	"time"

	"dialoguefit/sched"
)

func TestPoolRecordsQueueBackpressureAndPreservesResults(t *testing.T) {
	pool := sched.New(sched.Config{MaxParallel: 1, SampleBufferCap: 128, QueueBudget: 1})
	release := make(chan struct{})
	started := make(chan string, 2)
	tasks := []sched.Task{
		{ID: "a", BufferSamples: 16, Run: func(context.Context) error { started <- "a"; <-release; return nil }},
		{ID: "b", BufferSamples: 16, Run: func(context.Context) error { started <- "b"; <-release; return nil }},
		{ID: "c", BufferSamples: 16, Run: func(context.Context) error { started <- "c"; <-release; return nil }},
	}
	done := make(chan []sched.Result, 1)
	go func() { done <- pool.Run(context.Background(), tasks) }()

	select {
	case id := <-started:
		if id != "a" {
			t.Fatalf("first task=%q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("first task did not start")
	}
	time.Sleep(80 * time.Millisecond)
	close(release)

	select {
	case results := <-done:
		if len(results) != 3 {
			t.Fatalf("results=%d", len(results))
		}
		for _, r := range results {
			if r.Err != nil {
				t.Fatalf("task %s: %v", r.ID, r.Err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not finish")
	}
	events := pool.Events()
	found := false
	for _, ev := range events {
		if ev.Reason == "queue-budget-full" && ev.TaskID == "c" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected queue-budget-full event for c, got %+v", events)
	}
}

func TestPoolRejectsOversizedBufferWithoutDroppingTask(t *testing.T) {
	pool := sched.New(sched.Config{MaxParallel: 1, SampleBufferCap: 32, QueueBudget: 1})
	results := pool.Run(context.Background(), []sched.Task{{ID: "big", BufferSamples: 64, Run: func(context.Context) error { return nil }}})
	if results[0].Err == nil {
		t.Fatal("expected buffer budget error")
	}
}
