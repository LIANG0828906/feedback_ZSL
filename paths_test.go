package macrokit

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRealtimeReplayAndCheckpointResumeParity(t *testing.T) {
	set := GenerateSamples(FixedSampleSeed)
	for _, sample := range set.Projects {
		t.Run(sample.Slug, func(t *testing.T) {
			cfg := ExecutorConfig{FrameCount: sample.Truth.FrameCount}
			full, err := RunProject(sample.Project, sample.Events, cfg)
			if err != nil {
				t.Fatalf("full replay: %v", err)
			}
			if err := VerifyTruth(full, sample.Truth); err != nil {
				t.Fatalf("truth: %v", err)
			}

			realtime, err := RunRealtime(sample.Project, sample.Events, cfg)
			if err != nil {
				t.Fatalf("realtime: %v", err)
			}
			if err := CompareResults(full, realtime); err != nil {
				t.Fatalf("realtime mismatch: %v", err)
			}

			dbPath := filepath.Join(t.TempDir(), "checkpoint.sqlite")
			store, err := OpenSQLiteStore(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			stop := int64(0)
			first, err := RunCheckpointed(context.Background(), store, sample.Project, sample.Events, cfg, 4, nil, &stop)
			if err != nil {
				t.Fatalf("first checkpoint run: %v", err)
			}
			if !first.Stopped {
				t.Fatal("expected run to stop after first window")
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := OpenSQLiteStore(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			resumed, err := RunCheckpointed(context.Background(), reopened, sample.Project, sample.Events, cfg, 4, nil, nil)
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			if err := CompareResults(full, resumed.Result); err != nil {
				t.Fatalf("checkpoint resume mismatch: %v", err)
			}
		})
	}
}

func TestCheckpointFailureRetryCorruptionAndVersionInvalidation(t *testing.T) {
	ctx := context.Background()
	sample := findSample(t, GenerateSamples(FixedSampleSeed), "basic-timing")
	cfg := ExecutorConfig{FrameCount: sample.Truth.FrameCount}
	dbPath := filepath.Join(t.TempDir(), "atomic.sqlite")

	store, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	faults := &FaultInjector{FailAttempts: map[int64]int{1: 1}}
	_, err = RunCheckpointed(ctx, store, sample.Project, sample.Events, cfg, 4, faults, nil)
	if !HasCode(err, EFault) {
		t.Fatalf("expected injected E_FAULT, got %v", err)
	}

	committed, err := store.CountWindows(ctx, sample.Project.ID, "committed")
	if err != nil || committed != 1 {
		t.Fatalf("committed windows=%d err=%v", committed, err)
	}
	failed, err := store.CountWindows(ctx, sample.Project.ID, "failed")
	if err != nil || failed != 1 {
		t.Fatalf("failed windows=%d err=%v", failed, err)
	}
	snapshot, err := store.LoadSnapshot(ctx, sample.Project.ID, sample.Project.FormatNo, sample.Project.FormatVersion)
	if err != nil || snapshot == nil || len(snapshot.Windows) != 1 {
		t.Fatalf("other window snapshot should remain readable: %+v err=%v", snapshot, err)
	}

	resumed, err := RunCheckpointed(ctx, store, sample.Project, sample.Events, cfg, 4, nil, nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := VerifyTruth(resumed.Result, sample.Truth); err != nil {
		t.Fatalf("retried result truth: %v", err)
	}
	failed, _ = store.CountWindows(ctx, sample.Project.ID, "failed")
	if failed != 0 {
		t.Fatalf("failed rows after retry = %d, want 0", failed)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and corrupt a content hash.
	corruptStore, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := corruptStore.CorruptWindowHash(ctx, sample.Project.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, err = corruptStore.LoadSnapshot(ctx, sample.Project.ID, sample.Project.FormatNo, sample.Project.FormatVersion)
	if !HasCode(err, ECorrupt) {
		t.Fatalf("expected E_CORRUPT, got %v", err)
	}
	count, _ := corruptStore.CountWindows(ctx, sample.Project.ID, "")
	if count != 0 {
		t.Fatalf("corrupt snapshot should be fully invalidated, rows=%d", count)
	}

	// Repopulate, then corrupt the format version.
	_, err = RunCheckpointed(ctx, corruptStore, sample.Project, sample.Events, cfg, 4, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := corruptStore.CorruptWindowVersion(ctx, sample.Project.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, err = corruptStore.LoadSnapshot(ctx, sample.Project.ID, sample.Project.FormatNo, sample.Project.FormatVersion)
	if !HasCode(err, EVersion) {
		t.Fatalf("expected E_VERSION, got %v", err)
	}
	count, _ = corruptStore.CountWindows(ctx, sample.Project.ID, "")
	if count != 0 {
		t.Fatalf("version mismatch should invalidate all snapshot rows, rows=%d", count)
	}
	if err := corruptStore.Close(); err != nil {
		t.Fatal(err)
	}
}
