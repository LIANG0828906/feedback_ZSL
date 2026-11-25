package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestSQLiteCrashRollbackAndResume(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	fullSolver, err := NewSolver(scenario.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullSolver.Solve()
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := OpenSQLiteStore(path, WithFailpoint(func(phase string, tick int64) error {
		if phase == "before_global_tick" && tick == 15 {
			return errors.New("simulated abrupt termination")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExecutor(context.Background(), scenario.Canonical, store, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := executor.Run(context.Background())
	if err == nil {
		t.Fatal("expected injected commit failure")
	}
	if CodeOf(err) != CodeCheckpoint {
		t.Fatalf("unexpected failure code: %v", err)
	}
	if first.LastCommittedTick != 14 || executor.LastCommittedTick() != 14 {
		t.Fatalf("last complete commit = %d, want 14", executor.LastCommittedTick())
	}
	checkpoint, ok, err := store.LatestCheckpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok || checkpoint.Tick != 14 {
		t.Fatalf("persisted tick = %d (ok=%v), want 14", checkpoint.Tick, ok)
	}
	if len(checkpoint.Waiting) != 2 {
		t.Fatalf("waiting queue was not persisted: %+v", checkpoint.Waiting)
	}
	frozen := checkpointFrozen(checkpoint)
	if !stringSlicesEqual(frozen, []string{"wd", "we"}) {
		t.Fatalf("frozen workloads at tick 14 = %v", frozen)
	}
	if len(checkpoint.Domains) != 1 || checkpoint.Domains[0].ID != 200 || checkpoint.Domains[0].StartedTick != 12 {
		t.Fatalf("active domain was not persisted: %+v", checkpoint.Domains)
	}
	used := checkpointUsed(checkpoint)
	if used["wd"] != 0 || used["we"] != 1024 {
		t.Fatalf("queued budget was applied before commit: %v", used)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resumedExecutor, err := NewExecutor(context.Background(), scenario.Canonical, reopened, ExecutorOptions{Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if resumedExecutor.ResumeTick() != 15 {
		t.Fatalf("resume tick = %d, want 15", resumedExecutor.ResumeTick())
	}
	rest, err := resumedExecutor.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	combined := append(append([]TickResult(nil), first.Ticks...), rest.Ticks...)
	if !reflect.DeepEqual(full.Ticks, combined) {
		for index := range combined {
			if !reflect.DeepEqual(full.Ticks[index], combined[index]) {
				t.Fatalf("resumed result differs at tick %d", full.Ticks[index].Tick)
			}
		}
		t.Fatal("resumed run differs from uninterrupted run")
	}
	finalCheckpoint, ok, err := reopened.LatestCheckpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok || finalCheckpoint.Tick != 54 {
		t.Fatalf("final persisted tick = %d (ok=%v), want 54", finalCheckpoint.Tick, ok)
	}
	if len(finalCheckpoint.Domains) != 0 || len(finalCheckpoint.Waiting) != 0 {
		t.Fatalf("final checkpoint should be quiescent: %+v", finalCheckpoint)
	}
}

func TestGracefulInterruptionCommitsWholeTick(t *testing.T) {
	scenario, err := GenerateScenario(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	fullSolver, err := NewSolver(scenario.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullSolver.Solve()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "graceful.db")
	store, err := OpenSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executor, err := NewExecutor(context.Background(), scenario.Canonical, store, ExecutorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var committed []TickResult
	for tick := int64(0); tick < 3; tick++ {
		result, err := executor.Step(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		committed = append(committed, result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary, err := executor.Run(ctx)
	var interrupted *InterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("expected interrupted error, got %v", err)
	}
	if interrupted.LastCommittedTick != 2 || summary.LastCommittedTick != 2 {
		t.Fatalf("graceful stop did not preserve last commit: %+v", interrupted)
	}
	checkpoint, ok, err := store.LatestCheckpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok || checkpoint.Tick != 2 {
		t.Fatalf("persisted tick = %d (ok=%v), want 2", checkpoint.Tick, ok)
	}
	resumed, err := NewExecutor(context.Background(), scenario.Canonical, store, ExecutorOptions{Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	rest, err := resumed.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	combined := append(append([]TickResult(nil), committed...), rest.Ticks...)
	if !reflect.DeepEqual(full.Ticks, combined) {
		t.Fatal("graceful interruption plus resume differs from uninterrupted run")
	}
}

func checkpointFrozen(checkpoint Checkpoint) []string {
	var frozen []string
	for _, workload := range checkpoint.Workloads {
		if workload.Frozen {
			frozen = append(frozen, workload.WorkloadID)
		}
	}
	sort.Strings(frozen)
	return frozen
}

func checkpointUsed(checkpoint Checkpoint) map[string]int64 {
	used := make(map[string]int64, len(checkpoint.Workloads))
	for _, workload := range checkpoint.Workloads {
		used[workload.WorkloadID] = workload.UsedBytes
	}
	return used
}
