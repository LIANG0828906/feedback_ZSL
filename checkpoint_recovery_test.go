package chaosdiag_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"chaosdiag/checkpoint"
	"chaosdiag/engine"
	"chaosdiag/gen"
	"chaosdiag/model"
)

func TestSQLiteCheckpointStopAndResume(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "checkpoints", "demo.sqlite")
	store, err := checkpoint.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	stopped, err := checkpoint.RunFresh(ctx, bundle.Canonical, store, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.LastTick != 10 || !stopped.StoppedEarly {
		t.Fatalf("fresh run last=%d early=%v", stopped.LastTick, stopped.StoppedEarly)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cp, err := checkpoint.LoadExpected(path, bundle.Canonical.ProjectVersion)
	if err != nil {
		t.Fatal(err)
	}
	if cp.LastTick != 10 {
		t.Fatalf("checkpoint tick=%d want 10", cp.LastTick)
	}
	if len(cp.Queue) != 2 || cp.Queue[0].InstructionID != "I-Q3-PAY" || cp.Queue[1].InstructionID != "I-Q4-INV" {
		t.Fatalf("unexpected persisted queue: %+v", cp.Queue)
	}

	store2, err := checkpoint.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	resumed, err := checkpoint.Resume(ctx, bundle.Canonical, store2, *cp, 0)
	if err != nil {
		t.Fatal(err)
	}
	full := engine.Run(bundle.Canonical)
	if !reflect.DeepEqual(resumed.Report.Results, full.Results) {
		for i := range full.Results {
			if i >= len(resumed.Report.Results) {
				break
			}
			if !reflect.DeepEqual(resumed.Report.Results[i], full.Results[i]) {
				rb, _ := json.Marshal(resumed.Report.Results[i])
				fb, _ := json.Marshal(full.Results[i])
				t.Fatalf("result %s mismatch\nresumed=%s\nfull=%s", full.Results[i].RequestID, rb, fb)
			}
		}
		t.Fatal("resumed results differ from one-shot full replay")
	}
	if resumed.LastTick != 12 {
		t.Fatalf("resumed last tick=%d want 12", resumed.LastTick)
	}
}

func TestCancelAfterCommittedTickLeavesRecoverableCheckpoint(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signal-safe.sqlite")
	store, err := checkpoint.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out, err := checkpoint.RunFreshWithOptions(ctx, bundle.Canonical, store, checkpoint.RunOptions{
		AfterSave: func(tick int) {
			if tick == 3 {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
	if out == nil || out.LastTick != 3 {
		t.Fatalf("out=%+v want last complete tick 3", out)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cp, err := checkpoint.LoadExpected(path, bundle.Canonical.ProjectVersion)
	if err != nil {
		t.Fatal(err)
	}
	if cp.LastTick != 3 || len(cp.Requests) != 3 {
		t.Fatalf("checkpoint tick=%d requests=%d want 3/3", cp.LastTick, len(cp.Requests))
	}
}

func TestCheckpointVersionMismatchAndTruncationCodes(t *testing.T) {
	dir := t.TempDir()
	versionPath := filepath.Join(dir, "version.sqlite")
	store, err := checkpoint.OpenStore(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(checkpoint.Checkpoint{ProjectVersion: "stale-version", LastTick: 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.LoadExpected(versionPath, "current-version")
	ce, ok := model.AsError(err)
	if !ok || ce.Code != model.CodeCheckpointVersionMismatch {
		t.Fatalf("err=%v want %s", err, model.CodeCheckpointVersionMismatch)
	}

	truncPath := filepath.Join(dir, "truncated.sqlite")
	truncStore, err := checkpoint.OpenStore(truncPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := truncStore.Save(checkpoint.Checkpoint{ProjectVersion: "x", LastTick: 1}); err != nil {
		t.Fatal(err)
	}
	if err := truncStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(truncPath, 64); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.Load(truncPath)
	ce, ok = model.AsError(err)
	if !ok || ce.Code != model.CodeCheckpointCorrupt {
		t.Fatalf("truncated err=%v want %s", err, model.CodeCheckpointCorrupt)
	}
}

func TestVersionMismatchForcesFullReplay(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mismatch-recover.sqlite")
	store, err := checkpoint.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(checkpoint.Checkpoint{ProjectVersion: "older", LastTick: 9}); err != nil {
		t.Fatal(err)
	}
	out, err := checkpoint.RecoverOrRun(context.Background(), bundle.Canonical, store, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != "full_replay" {
		t.Fatalf("recovery mode=%s want full_replay", out.Mode)
	}
	full := engine.Run(bundle.Canonical)
	if !reflect.DeepEqual(out.Report.Results, full.Results) {
		t.Fatal("full replay after version mismatch differs")
	}
}
