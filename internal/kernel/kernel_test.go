package kernel_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"inputkernel/internal/kernel"
	"inputkernel/internal/model"
	"inputkernel/internal/samplegen"
	"inputkernel/internal/sampleio"
)

func TestGeneratedSamplesMatchTruth(t *testing.T) {
	scenarios, err := samplegen.Generate(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range scenarios {
		t.Run(scenario.ID, func(t *testing.T) {
			rt, err := kernel.NewRuntime(context.Background(), scenario, kernel.Options{MaxWorkers: 4, PerWorkerBuffer: 8})
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			if _, err := rt.RecomputeAll(); err != nil {
				t.Fatal(err)
			}
			result := rt.CompareTruth(kernel.DefaultTruthTolerance())
			if !result.Pass {
				t.Fatalf("truth mismatches: %+v", result.Mismatches)
			}
		})
	}
}

func TestIncrementalMatchesFullReplayAfterCurveEdit(t *testing.T) {
	scenarios, err := samplegen.Generate(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, original := range scenarios {
		t.Run(original.ID, func(t *testing.T) {
			incremental, err := kernel.NewRuntime(context.Background(), original, kernel.Options{MaxWorkers: 3, PerWorkerBuffer: 4})
			if err != nil {
				t.Fatal(err)
			}
			defer incremental.Close()
			if _, err := incremental.RecomputeAll(); err != nil {
				t.Fatal(err)
			}
			report, err := incremental.EditAndRecompute(func(s *model.Scenario) error {
				s.Curves[0].CurvaturePPM++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.ChangedNodes) == 0 {
				t.Fatal("edit produced no invalidation")
			}
			if report.ReusedNodes == 0 {
				t.Fatal("edit invalidated every node; expected unaffected node reuse")
			}
			edited := incremental.Scenario()
			full, err := kernel.NewRuntime(context.Background(), edited, kernel.Options{MaxWorkers: 3, PerWorkerBuffer: 4})
			if err != nil {
				t.Fatal(err)
			}
			defer full.Close()
			if _, err := full.RecomputeAll(); err != nil {
				t.Fatal(err)
			}
			comparison := kernel.CompareRuntimes(incremental, full)
			if !comparison.Equal {
				t.Fatalf("incremental differs from full: %+v", comparison.FirstDiff)
			}
			if err := incremental.ValidateSnapshot(mustSnapshot(t, incremental)); err != nil {
				t.Fatalf("edited runtime snapshot should validate: %v", err)
			}
		})
	}
}

func TestSnapshotRoundTripAndWholeInvalidation(t *testing.T) {
	scenario := loadScenario(t, "hot_switch")
	source, err := kernel.NewRuntime(context.Background(), scenario, kernel.Options{MaxWorkers: 2, PerWorkerBuffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.RecomputeAll(); err != nil {
		t.Fatal(err)
	}
	snapshot := mustSnapshot(t, source)
	store := kernel.NewSnapshotStore(t.TempDir())
	path, err := store.Save(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(snapshot.ScenarioID)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("snapshot path is empty")
	}
	restored, err := kernel.NewRuntime(context.Background(), scenario, kernel.Options{MaxWorkers: 2, PerWorkerBuffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.RestoreSnapshot(loaded); err != nil {
		t.Fatal(err)
	}
	if cmp := kernel.CompareRuntimes(source, restored); !cmp.Equal {
		t.Fatalf("restored snapshot differs: %+v", cmp.FirstDiff)
	}

	stale := *loaded
	stale.FixedPrecisionVersion = "stale"
	err = restored.RestoreSnapshot(&stale)
	var kernelErr *model.Error
	if !errors.As(err, &kernelErr) || kernelErr.Code != model.CodeVer {
		t.Fatalf("expected E_VER, got %v", err)
	}
	if len(restored.CacheEntries()) == 0 {
		t.Fatal("failed snapshot restore must not leave a partial cache")
	}
	if cmp := kernel.CompareRuntimes(source, restored); !cmp.Equal {
		t.Fatalf("failed restore changed committed cache: %+v", cmp.FirstDiff)
	}

	fresh, err := kernel.NewRuntime(context.Background(), scenario, kernel.Options{MaxWorkers: 2, PerWorkerBuffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.RestoreSnapshot(&stale); err == nil {
		t.Fatal("expected stale snapshot failure on fresh runtime")
	}
	if entries := fresh.CacheEntries(); len(entries) != 0 {
		t.Fatalf("stale snapshot partially populated %d nodes", len(entries))
	}
	if _, err := fresh.RecomputeAll(); err != nil {
		t.Fatal(err)
	}
	if cmp, err := kernel.CompareRuntimesChecked(source, fresh); err != nil || !cmp.Equal {
		t.Fatalf("full recovery after stale snapshot failed: %v %+v", err, cmp.FirstDiff)
	}
}

func TestConcurrentSnapshotWritesAreSerialized(t *testing.T) {
	scenario := loadScenario(t, "single_stick")
	rt, err := kernel.NewRuntime(context.Background(), scenario, kernel.Options{MaxWorkers: 2, PerWorkerBuffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rt.RecomputeAll(); err != nil {
		t.Fatal(err)
	}
	snapshot := mustSnapshot(t, rt)
	store := kernel.NewSnapshotStore(t.TempDir())
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Save(snapshot)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := store.Load(snapshot.ScenarioID)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.ValidateSnapshot(loaded); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerPoolRecordsBufferExhaustion(t *testing.T) {
	pool := kernel.NewWorkerPool(context.Background(), kernel.Options{MaxWorkers: 1, PerWorkerBuffer: 1})
	defer pool.Close()
	release := make(chan struct{})
	tasks := make([]kernel.Task, 0, 5)
	for i := 0; i < 5; i++ {
		id := string(rune('A' + i))
		tasks = append(tasks, kernel.Task{ID: id, Fn: func(context.Context) error {
			<-release
			return nil
		}})
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	if err := pool.Run(tasks); err != nil {
		t.Fatal(err)
	}
	stats := pool.Stats()
	if stats.BufferFullWaits == 0 {
		t.Fatalf("expected a buffer-full wait, got %+v", stats)
	}
	if len(stats.BufferFullRecords) == 0 || stats.BufferFullRecords[0].Reason != "per_worker_buffer_full" {
		t.Fatalf("missing buffer reason: %+v", stats)
	}
}

func TestDiffReturnsEDiffWithFirstEvent(t *testing.T) {
	leftScenario := loadScenario(t, "dual_stick")
	left, err := kernel.NewRuntime(context.Background(), leftScenario, kernel.Options{MaxWorkers: 2, PerWorkerBuffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	if _, err := left.RecomputeAll(); err != nil {
		t.Fatal(err)
	}
	rightScenario := loadScenario(t, "single_stick")
	right, err := kernel.NewRuntime(context.Background(), rightScenario, kernel.Options{MaxWorkers: 2, PerWorkerBuffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	if _, err := right.RecomputeAll(); err != nil {
		t.Fatal(err)
	}
	_, err = kernel.CompareRuntimesChecked(left, right)
	var kernelErr *model.Error
	if !errors.As(err, &kernelErr) || kernelErr.Code != model.CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	if kernelErr.FirstDiff == nil || kernelErr.FirstDiff.EventSeq == 0 {
		t.Fatalf("expected first differing event, got %+v", kernelErr.FirstDiff)
	}
}

func mustSnapshot(t *testing.T, rt *kernel.Runtime) *kernel.Snapshot {
	t.Helper()
	snapshot, err := rt.MakeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func loadScenario(t *testing.T, id string) *model.Scenario {
	t.Helper()
	scenarios, err := samplegen.Generate(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range scenarios {
		if scenario.ID == id {
			return sampleio.Clone(scenario)
		}
	}
	t.Fatalf("scenario %s not found", id)
	return nil
}
