package runtime_test

import (
	"os"
	"path/filepath"
	"testing"

	"schedlab/internal/domain"
	"schedlab/internal/faults"
	"schedlab/internal/gen"
	"schedlab/internal/runtime"
)

func TestDeterministicBackpressure(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	p := b.Truth.(*domain.Project)
	d, err := runtime.NewDeterministicRunner(p, runtime.Config{
		Concurrency: 2, BatchBuffer: 4096, RuleVersion: "r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Acquire(p.Pods[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Acquire(p.Pods[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Acquire(p.Pods[2].ID); !faults.Is(err, faults.E_BACKPRESSURE) {
		t.Fatalf("got %v want E_BACKPRESSURE", err)
	}
}

func TestIllegalBudgets(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	p := b.Truth.(*domain.Project)
	if _, err := runtime.NewRunner(p, mustStore(t), runtime.Config{
		Concurrency: 0, BatchBuffer: 8, RuleVersion: "r1",
	}); !faults.Is(err, faults.E_CONCURRENCY_BUDGET) {
		t.Fatalf("got %v want E_CONCURRENCY_BUDGET", err)
	}
	if _, err := runtime.NewRunner(p, mustStore(t), runtime.Config{
		Concurrency: 2, BatchBuffer: 0, RuleVersion: "r1",
	}); !faults.Is(err, faults.E_BUFFER_BUDGET) {
		t.Fatalf("got %v want E_BUFFER_BUDGET", err)
	}
}

func TestCheckpointRecoveryAndVersionMismatch(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	p := b.Truth.(*domain.Project)
	dir := filepath.Join(t.TempDir(), "cp")
	store, _ := runtime.OpenStore(dir)

	runner, err := runtime.NewRunner(p, store, runtime.Config{
		Concurrency: 4, BatchBuffer: 4096, RuleVersion: "r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := runner.Run()
	if err != nil {
		t.Fatal(err)
	}

	// Reopen as if after a forced termination.
	store2, _ := runtime.OpenStore(dir)
	frames, err := store2.LoadCache(first.SnapshotSeq)
	if err != nil {
		t.Fatal(err)
	}
	runner2, err := runtime.NewRunner(p, store2, runtime.Config{
		Concurrency: 4, BatchBuffer: 4096, RuleVersion: "r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := runner2.Run()
	if err != nil {
		t.Fatal(err)
	}
	if second.ReusedFromCheckpoint != len(frames) || len(frames) == 0 {
		t.Fatalf("recovery reuse = %d, frames = %d", second.ReusedFromCheckpoint, len(frames))
	}

	// Version mismatch must reject recovery.
	// Reuse the directory that already holds an r1 commit, then reopen with r2.
	store3, _ := runtime.OpenStore(filepath.Join(dir, "mismatch"))
	runnerR1, _ := runtime.NewRunner(p, store3, runtime.Config{
		Concurrency: 4, BatchBuffer: 4096, RuleVersion: "r1",
	})
	if _, err := runnerR1.Run(); err != nil {
		t.Fatal(err)
	}
	runner3, _ := runtime.NewRunner(p, store3, runtime.Config{
		Concurrency: 4, BatchBuffer: 4096, RuleVersion: "r2",
	})
	_, err = runner3.Run()
	if !faults.Is(err, faults.E_STATE_VERSION_MISMATCH) {
		t.Fatalf("got %v want E_STATE_VERSION_MISMATCH", err)
	}
}

func TestTruncatedCheckpointDetected(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	p := b.Truth.(*domain.Project)
	store, _ := runtime.OpenStore(t.TempDir())
	snap := runtime.BuildSnapshot(p, 1, "r1")
	if err := store.Commit(snap, []runtime.Frame{{
		PodID: p.Pods[0].ID, Selected: "x", Normalized: 1,
		RuleVersion: "r1", SnapshotSeq: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), "cache-1.json"),
		[]byte(`{"magic":"SCHEDLAB-CP","partial":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadCache(1); !faults.Is(err, faults.E_CHECKPOINT_TRUNCATED) {
		t.Fatalf("got %v want E_CHECKPOINT_TRUNCATED", err)
	}
}

func mustStore(t *testing.T) *runtime.Store {
	t.Helper()
	s, err := runtime.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
