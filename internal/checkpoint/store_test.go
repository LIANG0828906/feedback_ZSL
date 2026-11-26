package checkpoint_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"enginelab.local/internal/checkpoint"
	"enginelab.local/internal/model"
)

func TestCrashChild(t *testing.T) {
	if os.Getenv("ENGINELAB_CRASH_CHILD") != "1" {
		t.Skip("crash helper child only")
	}
	dbPath := os.Getenv("ENGINELAB_DB")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store.Hook = func(stage string) {
		if stage == "before_global_commit" {
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
		}
	}
	result := &model.RecoveryResult{Segments: []*model.SegmentReport{
		{SegmentID: "seg-000", State: model.StateReplayed, AppliedRecords: 3, CacheKey: "k0", LastRecovered: model.Point{LSN: 103, Offset: 3, Index: 2, RecordID: "r3"}, Range: model.ReplayRange{StartLSN: 100, EndLSN: 103, StartIndex: 0, EndIndex: 2, RecordCount: 3}},
		{SegmentID: "seg-001", State: model.StateReplayed, AppliedRecords: 2, CacheKey: "k1", LastRecovered: model.Point{LSN: 204, Offset: 4, Index: 4, RecordID: "r4"}, Range: model.ReplayRange{StartLSN: 200, EndLSN: 204, StartIndex: 0, EndIndex: 4, RecordCount: 5}},
	}}
	_, err = store.SaveRecovery(context.Background(), checkpoint.SaveRequest{
		CheckpointID:       "cp-crash",
		ProjectID:          "p-crash",
		GlobalCommittedLSN: 204,
		Result:             result,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestForcedKillBeforeGlobalCommitResumesConsistently(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "crash.sqlite")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.v")
	cmd.Env = append(os.Environ(), "ENGINELAB_CRASH_CHILD=1", "ENGINELAB_DB="+dbPath)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("child should have been SIGKILLed; output=%s", out)
	}

	ctx := context.Background()
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.LoadCommitted(ctx, model.CheckpointSchemaVersion)
	if !errors.Is(err, checkpoint.ErrNoCommittedCheckpoint) {
		t.Fatalf("expected no globally committed checkpoint after kill, got %v", err)
	}

	result := &model.RecoveryResult{CheckpointID: "cp-crash", Segments: []*model.SegmentReport{
		{SegmentID: "seg-000", State: model.StateReplayed, AppliedRecords: 3, CacheKey: "k0", LastRecovered: model.Point{LSN: 103, Offset: 3, Index: 2, RecordID: "r3"}, Range: model.ReplayRange{StartLSN: 100, EndLSN: 103, RecordCount: 3}},
		{SegmentID: "seg-001", State: model.StateReplayed, AppliedRecords: 5, CacheKey: "k1", LastRecovered: model.Point{LSN: 204, Offset: 4, Index: 4, RecordID: "r4"}, Range: model.ReplayRange{StartLSN: 200, EndLSN: 204, RecordCount: 5}},
	}}
	if _, err := store.SaveRecovery(ctx, checkpoint.SaveRequest{CheckpointID: "cp-crash", ProjectID: "p-crash", GlobalCommittedLSN: 204, Result: result}); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.LoadCommitted(ctx, model.CheckpointSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.GlobalCommittedLSN != 204 || len(persisted.Segments) != 2 {
		t.Fatalf("unexpected persisted checkpoint: %+v", persisted)
	}
	if persisted.Segments[1].Progress.AppliedCount != 5 || persisted.Segments[1].Progress.LastRecordID != "r4" {
		t.Fatalf("record progress not persisted: %+v", persisted.Segments[1].Progress)
	}

	bad, err := store.LoadCommitted(ctx, "recovery-checkpoint-v0")
	if err == nil {
		t.Fatalf("expected version mismatch, got %+v", bad)
	}
	var be *model.Error
	if !errors.As(err, &be) || be.Code != model.EVersionMismatch {
		t.Fatalf("expected E_VERSION_MISMATCH, got %v", err)
	}
}
