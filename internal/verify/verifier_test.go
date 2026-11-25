package verify_test

import (
	"context"
	"path/filepath"
	"testing"

	"semanticshading/internal/checkpoint"
	"semanticshading/internal/runner"
	"semanticshading/internal/sample"
	"semanticshading/internal/verify"
)

func TestVerifierDetectsTamperedBlockArea(t *testing.T) {
	s, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tampered.sqlite")
	id := runner.IdentityFor(s, s.Truth.TruthFingerprint)
	store, _, err := checkpoint.Open(path, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), s, store, runner.Config{Parallelism: 2, QueueBudget: 2}, nil); err != nil {
		t.Fatal(err)
	}
	checkpoints, err := store.LoadCheckpoints()
	if err != nil {
		t.Fatal(err)
	}
	tampered := checkpoints[3]
	if len(tampered.Block.Accumulators) == 0 {
		t.Fatal("block 3 has no accumulators")
	}
	regionID := tampered.Block.Accumulators[0].Label
	tampered.Block.Accumulators[0].Area++
	if err := store.Commit(context.Background(), tampered); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	readStore, err := checkpoint.OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readStore.Close()
	report, err := verify.Verify(context.Background(), readStore, s.Truth)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.First == nil {
		t.Fatal("tampered checkpoint was accepted")
	}
	if report.First.Field != "area" || report.First.BlockIndex != 3 || report.First.RegionID != regionID {
		t.Fatalf("first mismatch = %#v", report.First)
	}
}

func TestVerifierAcceptsCanonicalRun(t *testing.T) {
	s, _ := sample.Generate(sample.DefaultConfig())
	path := filepath.Join(t.TempDir(), "ok.sqlite")
	store, _, err := checkpoint.Open(path, runner.IdentityFor(s, s.Truth.TruthFingerprint))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), s, store, runner.Config{Parallelism: 1}, nil); err != nil {
		t.Fatal(err)
	}
	report, err := verify.Verify(context.Background(), store, s.Truth)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("canonical run rejected: %#v", report.Mismatches)
	}
}
