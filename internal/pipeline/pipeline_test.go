package pipeline_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/sample"
	"ocrworkbench/internal/store"
)

func TestThreePathsProduceIdenticalResults(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.Options{OutputDir: dir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	base := pipeline.Runner{
		Detector: core.DefaultDetectorParams(),
		Segment:  core.DefaultSegmentParams(),
	}
	full, err := base.Run(ctx, manifest, dir, "full")
	if err != nil {
		t.Fatalf("full: %v", err)
	}

	realtimeStore, err := store.Open(filepath.Join(dir, "realtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer realtimeStore.Close()
	realtimeRunner := base
	realtimeRunner.Store = realtimeStore
	realtime, err := realtimeRunner.Run(ctx, manifest, dir, "realtime")
	if err != nil {
		t.Fatalf("realtime: %v", err)
	}

	resumeStore, err := store.Open(filepath.Join(dir, "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer resumeStore.Close()
	resumeRunner := base
	resumeRunner.Store = resumeStore
	resumeRunner.Faults.CrashAfterTileCommits = 3
	_, err = resumeRunner.Run(ctx, manifest, dir, "resume")
	if !errors.Is(err, pipeline.ErrInjectedCrash) {
		t.Fatalf("first resume phase: got %v, want injected crash", err)
	}
	resumeRunner.Faults.CrashAfterTileCommits = 0
	resumeRunner.Faults.CrashAfterLineCommits = 2
	_, err = resumeRunner.Run(ctx, manifest, dir, "resume")
	if !errors.Is(err, pipeline.ErrInjectedCrash) {
		t.Fatalf("second resume phase: got %v, want injected crash", err)
	}
	resumeRunner.Faults.CrashAfterLineCommits = 0
	resumed, err := resumeRunner.Run(ctx, manifest, dir, "resume")
	if err != nil {
		t.Fatalf("resume completion: %v", err)
	}

	for name, candidate := range map[string]core.RunResult{"realtime": realtime, "resumed": resumed} {
		if differences := pipeline.CompareRuns(full, candidate); len(differences) > 0 {
			t.Fatalf("%s differs from full: %v", name, differences)
		}
	}
}

func TestFailedLineIsIsolatedAndRetryCompletesRemainingUnits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.Options{OutputDir: dir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	base := pipeline.Runner{
		Detector: core.DefaultDetectorParams(),
		Segment:  core.DefaultSegmentParams(),
	}
	clean, err := base.Run(ctx, manifest, dir, "full")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runner := base
	runner.Store = db
	runner.Faults.Line = func(imageID string, lineIndex int) error {
		if imageID == "dense_1280" && lineIndex == 1 {
			return errors.New("injected line recognition fault")
		}
		return nil
	}
	partial, err := runner.Run(ctx, manifest, dir, "realtime")
	if err == nil {
		t.Fatal("expected injected unit failure")
	}
	dense, ok := partial.Image("dense_1280")
	if !ok {
		t.Fatal("missing failed image result")
	}
	if dense.Lines[1].State != "failed" {
		t.Fatalf("target line state = %s", dense.Lines[1].State)
	}
	if dense.Lines[0].State != "committed" || dense.Lines[2].State != "committed" {
		t.Fatal("failure contaminated neighboring committed lines")
	}
	runner.Faults.Line = nil
	retried, err := runner.Run(ctx, manifest, dir, "realtime")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if differences := pipeline.CompareRuns(clean, retried); len(differences) > 0 {
		t.Fatalf("retry differs from clean full run: %v", differences)
	}
}

func TestWorkerPoolBoundsConcurrencyAndBuffers(t *testing.T) {
	ctx := context.Background()
	pool := pipeline.NewBufferPool(2, 32<<10)
	costs := make([]int64, 40)
	for i := range costs {
		costs[i] = 16 << 10
	}
	var active int32
	var maxActive int32
	var completed int32
	errs := pipeline.RunIndexed(ctx, len(costs), 2, costs, pool, func(index int) error {
		current := atomic.AddInt32(&active, 1)
		for {
			observed := atomic.LoadInt32(&maxActive)
			if current <= observed || atomic.CompareAndSwapInt32(&maxActive, observed, current) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		atomic.AddInt32(&completed, 1)
		atomic.AddInt32(&active, -1)
		if index == 7 {
			return errors.New("isolated unit error")
		}
		return nil
	})
	if completed != int32(len(costs)) {
		t.Fatalf("completed %d of %d units; queued units were dropped", completed, len(costs))
	}
	if maxActive > 2 {
		t.Fatalf("max concurrency %d exceeded 2", maxActive)
	}
	if errs[7] == nil {
		t.Fatal("unit error was not captured")
	}
	for i, err := range errs {
		if i != 7 && err != nil {
			t.Fatalf("unit %d contaminated by another error: %v", i, err)
		}
	}

	oversized := pipeline.NewBufferPool(1, 16<<10)
	result := pipeline.RunIndexed(ctx, 1, 1, []int64{32 << 10}, oversized, func(index int) error {
		t.Fatal("oversized unit must not execute")
		return nil
	})
	if !errors.Is(result[0], pipeline.ErrBufferLimit) {
		t.Fatalf("oversized buffer error = %v", result[0])
	}
}

func TestPauseAndCancelControl(t *testing.T) {
	control := pipeline.NewControl()
	control.Pause()
	done := make(chan error, 1)
	go func() {
		done <- control.Wait(context.Background())
	}()
	select {
	case err := <-done:
		t.Fatalf("paused wait returned early: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	control.Resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("resume did not release paused worker")
	}
	control.Cancel()
	if err := control.Wait(context.Background()); !errors.Is(err, pipeline.ErrCanceled) {
		t.Fatalf("canceled wait = %v", err)
	}
}

func TestPanicIsContainedToFailedLine(t *testing.T) {
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.Options{OutputDir: dir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	runner := pipeline.Runner{
		Detector: core.DefaultDetectorParams(),
		Segment:  core.DefaultSegmentParams(),
	}
	runner.Faults.Line = func(imageID string, lineIndex int) error {
		if imageID == "dense_1280" && lineIndex == 0 {
			panic("injected panic")
		}
		return nil
	}
	result, err := runner.Run(context.Background(), manifest, dir, "full")
	if err == nil {
		t.Fatal("expected panic to be reported as a unit failure")
	}
	dense, ok := result.Image("dense_1280")
	if !ok || dense.Lines[0].State != "failed" {
		t.Fatalf("panic was not contained to line 0: %+v", dense.Lines[0])
	}
	if dense.Lines[1].State != "committed" {
		t.Fatal("panic contaminated the next line")
	}
}
