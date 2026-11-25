package checkpoint_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"featurekernel/internal/checkpoint"
	"featurekernel/internal/engine"
	"featurekernel/internal/gen"
	"featurekernel/internal/kernel"
)

func TestSQLiteResumeAcrossRestartsAndBatchSizes(t *testing.T) {
	ctx := context.Background()
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := engine.New(project)
	if err != nil {
		t.Fatal(err)
	}
	want, err := expected.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "resume.sqlite")
	store, err := checkpoint.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := checkpoint.NewRunner(ctx, store, project, "")
	if err != nil {
		t.Fatal(err)
	}
	report, _, err := runner.Run(ctx, 20, 7)
	if err != nil {
		t.Fatal(err)
	}
	if report.CommittedSeq != 20 {
		t.Fatalf("first commit = %d", report.CommittedSeq)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := checkpoint.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	snapshot, err := store2.LoadSnapshot(ctx, project.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor != 20 {
		t.Fatalf("persisted cursor = %d", snapshot.Cursor)
	}
	runner2, err := checkpoint.NewRunner(ctx, store2, project, "")
	if err != nil {
		t.Fatal(err)
	}
	report2, got, err := runner2.Run(ctx, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if report2.StartCursor != 20 || report2.CommittedSeq != int64(len(project.Records)) {
		t.Fatalf("unexpected report: %+v", report2)
	}
	if err := kernel.CompareWindowResults(want.Windows, got.Windows, project.FixedTolerance); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want.RecomputedWindows, got.RecomputedWindows) {
		t.Fatalf("recomputed mismatch\nwant=%v\ngot =%v", want.RecomputedWindows, got.RecomputedWindows)
	}
	if !reflect.DeepEqual(want.Discarded, got.Discarded) {
		t.Fatalf("discarded mismatch")
	}
}

func TestTruncatedSQLiteReturnsStableCode(t *testing.T) {
	ctx := context.Background()
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "truncated.sqlite")
	store, err := checkpoint.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := checkpoint.NewRunner(ctx, store, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.Run(ctx, 20, 5); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(dbPath, info.Size()/2); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.OpenSQLite(ctx, dbPath)
	if code := kernel.CodeOf(err); code != kernel.CodeCheckpointTruncated && code != kernel.CodeCheckpointCorrupt {
		t.Fatalf("expected stable checkpoint corruption/truncation code, got %v", err)
	}
}

func TestSnapshotTruncationAndVersionMismatchCodes(t *testing.T) {
	ctx := context.Background()
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "snapshot.sqlite")
	store, err := checkpoint.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := checkpoint.NewRunner(ctx, store, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.Run(ctx, 0, 25); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ExportSnapshot(ctx, project.ProjectID, project.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	goodPath := filepath.Join(t.TempDir(), "good.checkpoint.json")
	if err := checkpoint.WriteSnapshotFile(goodPath, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.ReadSnapshotFile(goodPath); err != nil {
		t.Fatalf("good snapshot: %v", err)
	}

	truncatedPath := filepath.Join(t.TempDir(), "truncated.checkpoint.json")
	if err := checkpoint.WriteSnapshotFile(truncatedPath, snapshot); err != nil {
		t.Fatal(err)
	}
	truncateFileForTest(t, truncatedPath)
	_, err = checkpoint.ReadSnapshotFile(truncatedPath)
	if code := kernel.CodeOf(err); code != kernel.CodeCheckpointTruncated {
		t.Fatalf("truncated code = %s err=%v", code, err)
	}

	badVersion := *snapshot
	badVersion.CanonicalVersion = "canonical.v9"
	mismatchPath := filepath.Join(t.TempDir(), "mismatch.checkpoint.json")
	if err := checkpoint.WriteSnapshotFile(mismatchPath, &badVersion); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.ReadSnapshotFile(mismatchPath)
	if code := kernel.CodeOf(err); code != kernel.CodeCheckpointVersionMismatch {
		t.Fatalf("mismatch code = %s err=%v", code, err)
	}
}

func TestSQLiteVersionMismatchForcesFullRecompute(t *testing.T) {
	ctx := context.Background()
	project, err := gen.BuildProject(gen.DefaultSeed, 1)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "mismatch.sqlite")
	store, err := checkpoint.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := checkpoint.NewRunner(ctx, store, project, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.Run(ctx, 30, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE checkpoint_meta SET value='checkpoint.v9' WHERE key='checkpoint_version'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := checkpoint.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, err = checkpoint.NewRunner(ctx, reopened, project, "")
	if code := kernel.CodeOf(err); code != kernel.CodeCheckpointVersionMismatch {
		t.Fatalf("expected mismatch, got %v", err)
	}
	reset, err := checkpoint.ResetMismatchedCheckpoint(ctx, reopened, project)
	if err != nil {
		t.Fatal(err)
	}
	if !reset {
		t.Fatalf("mismatch should be reset")
	}
	runner2, err := checkpoint.NewRunner(ctx, reopened, project, "")
	if err != nil {
		t.Fatal(err)
	}
	report, got, err := runner2.Run(ctx, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if report.StartCursor != 0 {
		t.Fatalf("version mismatch must recompute from zero, start=%d", report.StartCursor)
	}
	eng, _ := engine.New(project)
	want, err := eng.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := kernel.CompareWindowResults(want.Windows, got.Windows, project.FixedTolerance); err != nil {
		t.Fatal(err)
	}
}
