package replaybench_test

import (
	"context"
	"testing"
	"time"

	"replaybench/internal/checkpoint"
	"replaybench/internal/diff"
	"replaybench/internal/engine"
	"replaybench/internal/model"
	"replaybench/internal/runtime"
	"replaybench/internal/scheduler"
)

func TestCheckpointResumeVersionInvalidationAndThreePaths(t *testing.T) {
	loaded := loadSample(t, model.FormatWeightCast)
	replay := loaded.Replay
	full, err := engine.Run(context.Background(), replay)
	if err != nil {
		t.Fatal(err)
	}

	dbPath, cleanup, err := checkpoint.MustTempDB()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := scheduler.New(scheduler.Config{MaxWorkers: 2, WorkerBuffer: 1, QueueBudget: 4})
	mgr := &runtime.Manager{Store: store, Pool: pool, WindowSize: 2}
	realTime, err := mgr.Run(context.Background(), replay)
	if err != nil {
		t.Fatal(err)
	}
	if err := diff.Snapshots(full.Snapshots, realTime.Result.Snapshots); err != nil {
		t.Fatalf("checkpoint path differs from full: %v", err)
	}
	if got := realTime.CommitSequence; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("commit sequence = %v", got)
	}
	if err := pool.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	resumeMgr := &runtime.Manager{Store: store2, WindowSize: 2}
	resumed, err := resumeMgr.Run(context.Background(), replay)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.RecoveredWindow == nil || resumed.RecoveredWindow.Index != 2 {
		t.Fatalf("recovered = %+v", resumed.RecoveredWindow)
	}
	if resumed.StartRound != replay.Rounds+1 {
		t.Fatalf("start round = %d", resumed.StartRound)
	}
	if err := diff.Snapshots(full.Snapshots, resumed.Result.Snapshots); err != nil {
		t.Fatalf("resume differs: %v", err)
	}
	if err := diff.CommitSequences(realTime.CommitSequence, resumed.CommitSequence); err != nil {
		t.Fatal(err)
	}

	badIdentity := checkpoint.IdentityFromReplay(replay)
	badIdentity.ParamsVersion = "params-does-not-match"
	last, err := store2.Recover(badIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if last != nil {
		t.Fatal("version-mismatched checkpoint must be invalidated")
	}
	indexes, err := store2.ListWindowIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 0 {
		t.Fatalf("mismatched windows remain: %v", indexes)
	}
}

func TestCancelBeforeCommitLeavesNoPartialWindow(t *testing.T) {
	loaded := loadSample(t, model.FormatWeightCast)
	dbPath, cleanup, _ := checkpoint.MustTempDB()
	defer cleanup()
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mgr := &runtime.Manager{Store: store, WindowSize: 1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = mgr.Run(ctx, loaded.Replay)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	indexes, _ := store.ListWindowIndexes()
	if len(indexes) != 0 {
		t.Fatalf("canceled run committed windows: %v", indexes)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	report, err := mgr.Run(ctx2, loaded.Replay)
	if err != nil {
		t.Fatal(err)
	}
	full, _ := engine.Run(context.Background(), loaded.Replay)
	if err := diff.Snapshots(full.Snapshots, report.Result.Snapshots); err != nil {
		t.Fatal(err)
	}
}
