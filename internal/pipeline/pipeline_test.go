package pipeline_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"battlekernel/internal/checkpoint"
	"battlekernel/internal/errs"
	"battlekernel/internal/pipeline"
	"battlekernel/internal/sample"
)

func TestThreePathsAndHashCorruptionReset(t *testing.T) {
	ctx := context.Background()
	suite, err := sample.GenerateSuite(sample.DefaultSeed, 1)
	if err != nil {
		t.Fatal(err)
	}
	p := suite.Samples[0].Project
	dir := t.TempDir()

	full, err := pipeline.FullRecompute(p, pipeline.RunOptions{WindowSize: 2})
	if err != nil {
		t.Fatal(err)
	}

	realtimePath := filepath.Join(dir, "realtime.db")
	store, err := checkpoint.Open(realtimePath)
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := pipeline.RunRealtime(ctx, p, store, pipeline.RunOptions{WindowSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if diff := pipeline.CompareReports(p.ID, full, realtime); !diff.Equal {
		t.Fatalf("realtime mismatch: %+v", diff.Difference)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	breakPath := filepath.Join(dir, "break.db")
	breakStore, err := checkpoint.Open(breakPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunRealtime(ctx, p, breakStore, pipeline.RunOptions{WindowSize: 2, StopAfterWindows: 1}); err != nil {
		t.Fatal(err)
	}
	if err := breakStore.Close(); err != nil {
		t.Fatal(err)
	}
	resumeStore, err := checkpoint.Open(breakPath)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := pipeline.RunResume(ctx, p, resumeStore, pipeline.RunOptions{WindowSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if diff := pipeline.CompareReports(p.ID, full, resume); !diff.Equal {
		t.Fatalf("resume mismatch: %+v", diff.Difference)
	}
	if err := resumeStore.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", realtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE rounds SET content_hash='bad' WHERE project_id=? AND round=1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	corruptStore, err := checkpoint.Open(realtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := corruptStore.ValidateSnapshot(ctx, p.ID); !errs.IsCode(err, errs.ECheck) {
		t.Fatalf("expected E_CHECKPOINT hash error, got %v", err)
	}
	recovered, err := pipeline.RunResume(ctx, p, corruptStore, pipeline.RunOptions{WindowSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if diff := pipeline.CompareReports(p.ID, full, recovered); !diff.Equal {
		t.Fatalf("recovery mismatch: %+v", diff.Difference)
	}
	if err := corruptStore.Close(); err != nil {
		t.Fatal(err)
	}
}
