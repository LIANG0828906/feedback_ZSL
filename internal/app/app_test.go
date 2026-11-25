package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"graphlab/internal/app"
	"graphlab/internal/generator"
	"graphlab/internal/model"
)

func testOptions(t *testing.T) app.Options {
	t.Helper()
	dir := t.TempDir()
	return app.Options{
		SampleDir: filepath.Join(dir, "sample"), CheckpointPath: filepath.Join(dir, "checkpoint.db"),
		OutputDir: dir, Workers: 4, BufferPerWorker: 4096, Seed: generator.DefaultSeed,
	}
}

func TestValidateDemoAndFaultsEndToEnd(t *testing.T) {
	opts := testOptions(t)
	validation, err := app.Validate(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !validation.AllPassed {
		t.Fatalf("validation failed: %+v", validation.Checks)
	}
	demo, err := app.RunDemo(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(demo.Operations) != 7 || !demo.IncrementalFullEqual || !demo.OperationTruthMatched ||
		len(demo.RecoveryQueryDiffs) != 0 ||
		demo.ForcedMismatchCode != model.ECheckpointVersionMismatch {
		t.Fatalf("demo result incomplete: %+v", demo)
	}
	faults, err := app.RunFaults(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !faults.AllExpected || !faults.SourceUnchanged ||
		!faults.CommittedRuntimeStillUsable || !faults.OriginalCheckpointUsable {
		t.Fatalf("fault validation failed: %+v", faults)
	}
}

func TestDemoCancellationLeavesLastCompleteCommit(t *testing.T) {
	opts := testOptions(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := app.RunDemo(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !report.StoppedOnSignal {
		t.Fatal("demo did not observe cancellation")
	}
	if report.FinalHeader.CommitSeq != 0 {
		t.Fatalf("expected last complete commit seq=0, got %d", report.FinalHeader.CommitSeq)
	}
	if len(report.RecoveryQueryDiffs) != 0 {
		t.Fatalf("recovery after cancellation differs: %v", report.RecoveryQueryDiffs)
	}
}
