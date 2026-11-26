package strokelab

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSchedulerQueueBudgetPixelLimitAndRetry(t *testing.T) {
	scheduler, err := NewScheduler(SchedulerConfig{
		MaxParallelism: 1, MaxPixelsPerWorker: 16, QueueBudget: 1, MaxRetries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	failed := make(map[string]bool)
	scheduler.FailureInjector = func(task BlockTask, attempt int) error {
		if task.ID == "task-03" && attempt == 1 {
			mu.Lock()
			failed[task.ID] = true
			mu.Unlock()
			return errors.New("injected once")
		}
		return nil
	}
	var tasks []BlockTask
	for i := 0; i < 8; i++ {
		id := i
		tasks = append(tasks, BlockTask{
			ID: "task-0" + string(rune('0'+id)), StrokeID: "s", StrokeOrdinal: 0,
			BlockID: i, Block: Rect{X: i, Y: 0, W: 4, H: 4},
			Run: func() ([]uint8, error) {
				time.Sleep(2 * time.Millisecond)
				return make([]uint8, 16), nil
			},
		})
	}
	results, err := scheduler.RunBatch(context.Background(), tasks)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(results) != 8 {
		t.Fatalf("results=%d", len(results))
	}
	var sawQueue, sawFail, sawSuccess bool
	for _, rec := range scheduler.Records() {
		if rec.QueueReason == "queue_budget" {
			sawQueue = true
		}
		if rec.Outcome == "failed" {
			sawFail = true
		}
		if rec.Outcome == "succeeded" {
			sawSuccess = true
		}
	}
	if !sawQueue || !sawFail || !sawSuccess {
		t.Fatalf("missing queue/retry evidence: queue=%v fail=%v success=%v records=%+v",
			sawQueue, sawFail, sawSuccess, scheduler.Records())
	}
	bad := []BlockTask{{
		ID: "too-big", BlockID: 0, Block: Rect{X: 0, Y: 0, W: 8, H: 8},
		Run: func() ([]uint8, error) { return nil, nil },
	}}
	if _, err := scheduler.RunBatch(context.Background(), bad); ErrorCode(err) != CodeBudget {
		t.Fatalf("expected E_BUDGET, got %v", err)
	}
}

func TestFailureAfterCommittedStrokeDoesNotPolluteCheckpoint(t *testing.T) {
	suite, err := GenerateSuite(8080)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Samples[2].Project
	fullEngine, _ := NewEngine(project)
	full, err := fullEngine.Run(context.Background(), RunOptions{Mode: RunFull})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := OpenCheckpoint(t.TempDir() + "/fail.ckpt")
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	engine, _ := NewEngine(project)
	_, runErr := engine.Run(context.Background(), RunOptions{
		Mode: RunIncremental, RunID: "fail", Checkpoint: cp,
		FailureInjector: func(task BlockTask, attempt int) error {
			if task.StrokeOrdinal == 2 {
				return errors.New("persistent block failure")
			}
			return nil
		},
	})
	if ErrorCode(runErr) != CodeExecution {
		t.Fatalf("expected E_EXEC, got %v", runErr)
	}
	if got := len(cp.Records()); got != 2 {
		t.Fatalf("checkpoint committed %d records, want 2", got)
	}
	resumeEngine, _ := NewEngine(project)
	resumed, err := resumeEngine.Run(context.Background(), RunOptions{
		Mode: RunResume, RunID: "fail", Checkpoint: cp,
	})
	if err != nil {
		t.Fatalf("resume after failure: %v", err)
	}
	if cmp := CompareEvaluations(full, resumed); !cmp.Equal {
		t.Fatalf("resume after failed stroke differs: %+v", cmp.Mismatches)
	}
}
