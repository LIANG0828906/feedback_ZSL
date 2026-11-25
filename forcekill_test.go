package replaybench_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"replaybench/internal/checkpoint"
	"replaybench/internal/diff"
	"replaybench/internal/engine"
	"replaybench/internal/generator"
	"replaybench/internal/loader"
	"replaybench/internal/model"
	"replaybench/internal/runtime"
)

func TestForceKillCheckpointRecoveryBoundary(t *testing.T) {
	if os.Getenv("RB_CRASH_HELPER") == "1" {
		runCrashHelper(t)
		return
	}
	dbPath := filepath.Join(t.TempDir(), "force-kill.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestForceKillCheckpointRecoveryBoundary$")
	cmd.Env = append(os.Environ(), "RB_CRASH_HELPER=1", "RB_CRASH_DB="+dbPath)
	err := cmd.Run()
	if err == nil {
		t.Fatal("crash helper unexpectedly exited cleanly")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %v", err)
	}
	waitStatus, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !waitStatus.Signaled() || waitStatus.Signal() != syscall.SIGKILL {
		t.Fatalf("expected SIGKILL, got %v", exitErr.ProcessState)
	}

	loaded := loadSample(t, model.FormatWeightCast)
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity := checkpoint.IdentityFromReplay(loaded.Replay)
	last, err := store.Recover(identity)
	if err != nil {
		t.Fatal(err)
	}
	if last == nil || last.Index != 1 {
		t.Fatalf("persisted boundary = %+v, want window 1", last)
	}
	mgr := &runtime.Manager{Store: store, WindowSize: 1}
	resumed, err := mgr.Run(context.Background(), loaded.Replay)
	if err != nil {
		t.Fatal(err)
	}
	full, err := engine.Run(context.Background(), loaded.Replay)
	if err != nil {
		t.Fatal(err)
	}
	if err := diff.Snapshots(full.Snapshots, resumed.Result.Snapshots); err != nil {
		t.Fatal(err)
	}
	if len(resumed.CommitSequence) != 3 {
		t.Fatalf("commit sequence = %v", resumed.CommitSequence)
	}
}

func runCrashHelper(t *testing.T) {
	dbPath := os.Getenv("RB_CRASH_DB")
	if dbPath == "" {
		t.Fatal("RB_CRASH_DB missing")
	}
	set, err := generator.DeterministicSet(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loader.LoadJSON(set.JSON[model.FormatWeightCast])
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mgr := &runtime.Manager{Store: store, WindowSize: 1}
	mgr.AfterCommit = func(index int) {
		if index == 1 {
			_ = store.Close()
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
	}
	if _, err := mgr.Run(context.Background(), loaded.Replay); err != nil {
		t.Fatal(err)
	}
}
