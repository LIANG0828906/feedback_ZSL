package workbench_test

import (
	"context"
	"path/filepath"
	"testing"

	"tacticalworkbench/internal/sample"
	"tacticalworkbench/pkg/tactical"
	"tacticalworkbench/pkg/workbench"
)

func TestServicePauseEditRestartAndRetry(t *testing.T) {
	scenario, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "service.db")
	service := workbench.NewServiceWithScenario(scenario, dbPath)
	state, err := service.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Snapshot == nil || !state.Checkpoint.Valid {
		t.Fatalf("service did not initialize a valid snapshot: %+v", state)
	}
	if _, err := service.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetCellCost(context.Background(), 10, 5, 3); err == nil {
		t.Fatal("expected paused service to reject edits")
	}
	if _, err := service.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	edit, err := service.SetCellCost(context.Background(), 10, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	beforeRestart := edit.State.Snapshot
	state, err = service.Restart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tactical.CompareSnapshots(beforeRestart, state.Snapshot); err != nil {
		t.Fatalf("restart did not resume the edited snapshot: %v", err)
	}
	retried, err := service.RetryMember(context.Background(), "A-REAR")
	if err != nil {
		t.Fatal(err)
	}
	if retried.Snapshot == nil {
		t.Fatal("retry did not return a snapshot")
	}
	service.Shutdown()
}
