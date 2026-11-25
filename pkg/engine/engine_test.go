package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"roomacoustics/pkg/acoustics"
	"roomacoustics/pkg/checkpoint"
	"roomacoustics/pkg/engine"
	"roomacoustics/pkg/generator"
)

func TestRoomFailureIsolatedThenRetryCommits(t *testing.T) {
	spec, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	eng, err := engine.New(spec.Level, engine.Config{Store: store, MaxWorkers: 2, BufferPerWorker: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	eng.FailNext("R2", 1)

	report, err := eng.Run(context.Background())
	if !errors.Is(err, engine.ErrRoomsFailed) {
		t.Fatalf("expected ErrRoomsFailed, got %v", err)
	}
	if report.Progress.Committed != 3 || report.Progress.Failed != 1 {
		t.Fatalf("expected 3 committed and 1 failed, got %+v", report.Progress)
	}
	states := map[string]acoustics.State{}
	for _, r := range report.Rooms {
		states[r.RoomID] = r.State
	}
	if states["R2"] != acoustics.StateFailed {
		t.Fatalf("R2 state = %s", states["R2"])
	}
	for _, id := range []string{"R1", "R3", "R4"} {
		if states[id] != acoustics.StateCommitted {
			t.Fatalf("%s state = %s", id, states[id])
		}
	}

	retryReport, err := eng.RetryFailed(context.Background())
	if err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	if retryReport.Progress.Committed != 4 || retryReport.Progress.Failed != 0 {
		t.Fatalf("retry progress = %+v", retryReport.Progress)
	}
	count, err := store.CountCheckpoints(context.Background(), retryReport.GlobalHash)
	if err != nil || count != 4 {
		t.Fatalf("checkpoint count=%d err=%v", count, err)
	}
}

func TestRestartRecoveryRequiresMatchingSignature(t *testing.T) {
	spec, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "recovery.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(spec.Level, engine.Config{Store: store, MaxWorkers: 2, BufferPerWorker: 4, StopAfterCommits: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background()); !errors.Is(err, engine.ErrInterrupted) {
		t.Fatalf("expected interruption, got %v", err)
	}
	eng.Close()
	order, err := store.CommitOrder(context.Background(), eng.GlobalHash())
	if err != nil || len(order) != 2 {
		t.Fatalf("partial commit order=%v err=%v", order, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	eng2, err := engine.New(spec.Level, engine.Config{Store: reopened, MaxWorkers: 2, BufferPerWorker: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	report, err := eng2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Progress.Committed != 4 {
		t.Fatalf("recovery report progress=%+v", report.Progress)
	}
	order, err = reopened.CommitOrder(context.Background(), report.GlobalHash)
	if err != nil || stringsJoin(order) != "R1|R2|R3|R4" {
		t.Fatalf("stable commit order=%v err=%v", order, err)
	}

	ws, err := reopened.InitializeWindow(context.Background(), checkpoint.Signature{GlobalHash: "different", ParamVersion: acoustics.ParamVersion, AlgorithmVersion: acoustics.AlgorithmVersion}, []string{"R1", "R2", "R3", "R4"})
	if err != nil {
		t.Fatal(err)
	}
	if !ws.Discarded {
		t.Fatal("version/hash mismatch must discard old snapshots")
	}
	count, _ := reopened.CountCheckpoints(context.Background(), "different")
	if count != 0 {
		t.Fatalf("mismatched window reused %d checkpoints", count)
	}
}

func stringsJoin(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "|"
		}
		out += x
	}
	return out
}
