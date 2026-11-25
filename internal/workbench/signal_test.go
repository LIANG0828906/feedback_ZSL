package workbench_test

import (
	"context"
	"path/filepath"
	"testing"

	"mvccworkbench/internal/checkpoint"
	"mvccworkbench/internal/domain"
	"mvccworkbench/internal/engine"
	"mvccworkbench/internal/generator"
	"mvccworkbench/internal/workbench"
)

func TestCancelStopsAtLastCompleteCheckpoint(t *testing.T) {
	suite, err := generator.DefaultSuite(424242)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Batches["B1-visibility"].Project
	dbPath := filepath.Join(t.TempDir(), "signal.db")
	ctx, cancel := context.WithCancel(context.Background())

	report, runErr := workbench.RunCheckpointed(ctx, project, dbPath, engine.DefaultOptions(), func(seq int) {
		if seq == 4 {
			cancel()
		}
	})
	if domain.CodeOf(runErr) != domain.EInterrupted {
		t.Fatalf("err=%v code=%s", runErr, domain.CodeOf(runErr))
	}
	if report == nil || report.FinalSeq != 4 {
		t.Fatalf("report=%#v want final seq 4", report)
	}

	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.CommittedOpSeq != 4 {
		t.Fatalf("checkpoint seq=%d want 4", state.CommittedOpSeq)
	}
}
