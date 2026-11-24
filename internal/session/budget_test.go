package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"spatialworkbench/internal/werr"
)

func TestWorkerPoolBudgetBlocksAndLogs(t *testing.T) {
	budget := newSampleBudget(100)
	if err := budget.Acquire(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if err := budget.Acquire(ctx, 100); err == nil || err.Code != werr.CodeFrame {
		t.Fatalf("expected canceled blocked acquire, got %#v", err)
	}
	events := budget.Events()
	if len(events) == 0 || !strings.Contains(events[0].Reason, "sample-buffer-exhausted") {
		t.Fatalf("expected a buffer-block event, got %#v", events)
	}
	budget.Release(100)
	pool := newWorkerPool(2, 100)
	tasks := []func(context.Context) (any, *werr.Error){
		func(context.Context) (any, *werr.Error) { return nil, nil },
	}
	tooLarge := []func(context.Context) (any, *werr.Error){tasks[0]}
	if _, err := pool.Run(context.Background(), tooLarge, []int{101}); err == nil || err.Code != werr.CodeFrame {
		t.Fatalf("expected per-worker limit error, got %#v", err)
	}
}
