package engine_test

import (
	"context"
	"path/filepath"
	"testing"

	"audioforge/internal/engine"
	"audioforge/internal/errs"
	"audioforge/internal/generator"
	"audioforge/internal/model"
	"audioforge/internal/store"
	"audioforge/internal/verify"
)

func TestRealtimeResumeAndReplayAreConsistent(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p-triple", AlgorithmVersion: "a-triple", CommitWindow: 3, Workers: 3, BufferPerWorker: 2}
	db, err := store.Open(filepath.Join(t.TempDir(), "triple.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runner := &engine.Engine{Store: db}
	realtime, err := runner.Run(context.Background(), "realtime", bundle.CanonicalEvents, bundle.Capability, params, engine.RunOptions{Persist: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := verify.Truth(realtime, bundle.Truth, bundle.Capability); err != nil {
		t.Fatal(err)
	}

	partial, err := runner.Run(context.Background(), "recovered", bundle.CanonicalEvents, bundle.Capability, params, engine.RunOptions{Persist: true, ForceAfterWindows: 2})
	if !errs.IsCode(err, errs.ETerminated) {
		t.Fatalf("expected E_TERMINATED, got %v", err)
	}
	if len(partial.Commits) != 2 {
		t.Fatalf("forced run committed %d windows", len(partial.Commits))
	}
	if !db.Exists("recovered") {
		t.Fatal("forced run did not leave a recoverable snapshot")
	}

	resumed, err := runner.Resume(context.Background(), "recovered", bundle.CanonicalEvents, bundle.Capability, params, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Compare("realtime", realtime, "resume", resumed); err != nil {
		t.Fatal(err)
	}
	replayed, err := runner.Replay("recovered", params)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Compare("realtime", realtime, "replay", replayed); err != nil {
		t.Fatal(err)
	}
	if len(replayed.Events) != 0 {
		t.Fatal("replay should not depend on in-memory events")
	}
}

func TestCancelAndForcedTerminationAreDistinct(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 2, Workers: 1, BufferPerWorker: 1}
	db, err := store.Open(filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runner := &engine.Engine{Store: db}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runner.Run(ctx, "cancelled", bundle.CanonicalEvents, bundle.Capability, params, engine.RunOptions{Persist: true})
	if !errs.IsCode(err, errs.ECancel) {
		t.Fatalf("expected E_CANCEL, got %v", err)
	}
	if db.Exists("cancelled") {
		t.Fatal("actively cancelled run should not leave a resumable snapshot")
	}
	_, err = runner.Run(context.Background(), "forced", bundle.CanonicalEvents, bundle.Capability, params, engine.RunOptions{Persist: true, ForceAfterWindows: 1})
	if !errs.IsCode(err, errs.ETerminated) {
		t.Fatalf("expected E_TERMINATED, got %v", err)
	}
	if !db.Exists("forced") {
		t.Fatal("forced termination should leave committed windows")
	}
}

func TestCompareLocatesFirstSampleAndPacket(t *testing.T) {
	bundle, _ := generator.Generate(generator.DefaultSeed)
	params := model.BuildParameters{ParamVersion: "p", AlgorithmVersion: "a", CommitWindow: 2, Workers: 1, BufferPerWorker: 1}
	left, err := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	right, err := (&engine.Engine{}).Build(bundle.CanonicalEvents, bundle.Capability, params)
	if err != nil {
		t.Fatal(err)
	}
	targetOffset := int64(37)
	right.Frames[0].Samples[targetOffset]++
	err = engine.Compare("left", left, "right", right)
	if !errs.IsCode(err, errs.EDiff) {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	coded := err.(*errs.Error)
	if coded.SampleOffset != targetOffset || coded.Channel != 0 {
		t.Fatalf("unexpected mismatch location: %+v", coded)
	}
	if coded.PacketSeq != 0 {
		t.Fatalf("packet sequence = %d, want 0", coded.PacketSeq)
	}
}
