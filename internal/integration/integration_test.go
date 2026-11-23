package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"roomreverb/internal/engine"
	"roomreverb/internal/generator"
	"roomreverb/internal/model"
	"roomreverb/internal/solver"
	"roomreverb/internal/validate"
	"roomreverb/internal/verify"
)

func TestDeterministicSampleAndThreeWay(t *testing.T) {
	a, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	b, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Level.Canonical()) != string(b.Level.Canonical()) {
		t.Fatal("fixed seed did not reproduce level")
	}
	if err := validate.Validate(a.Level); err != nil {
		t.Fatal(err)
	}
	cfg := engine.Config{
		Solvers: solver.Config{MaxParallel: 3, PathBufferLimit: 64, GraphCacheBudget: 2},
	}
	three, err := verify.ThreeWay(context.Background(), a.Level, a.Edits.Edits, cfg, filepath.Join(t.TempDir(), "check.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Compare(three.Full, three.Preview); err != nil {
		t.Fatal(err)
	}
	if err := engine.Compare(three.Full, three.Resume); err != nil {
		t.Fatal(err)
	}
	if three.Full.Progress.Committed != len(a.Level.Rooms) {
		t.Fatalf("committed %d/%d", three.Full.Progress.Committed, three.Full.Progress.Total)
	}
	if err := verify.VerifyTruth(a.Level, a.Truth, three.Base); err != nil {
		t.Fatal(err)
	}
}

func TestTruthPairsAreReachable(t *testing.T) {
	b, err := generator.Generate(7)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range b.Truth.Pairs {
		if !p.Reachable {
			t.Fatalf("sample pair %s/%s unexpectedly unreachable", p.SourceID, p.ListenerID)
		}
	}
}

func TestMultipleSeedsValidateAndMatchTruth(t *testing.T) {
	for seed := int64(0); seed < 16; seed++ {
		b, err := generator.Generate(seed)
		if err != nil {
			t.Fatalf("seed %d generation: %v", seed, err)
		}
		if err := validate.Validate(b.Level); err != nil {
			t.Fatalf("seed %d validation: %v", seed, err)
		}
		run, err := engine.RunFull(context.Background(), b.Level, engine.Config{
			Solvers: solver.Config{MaxParallel: 3, PathBufferLimit: 64, GraphCacheBudget: 2},
		})
		if err != nil {
			t.Fatalf("seed %d solve: %v", seed, err)
		}
		if err := verify.VerifyTruth(b.Level, b.Truth, run); err != nil {
			t.Fatalf("seed %d truth: %v", seed, err)
		}
	}
}

func TestPreviewOnlyInvalidatesDownstream(t *testing.T) {
	b, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := engine.NewPreview(ctx, b.Level, nil, engine.Config{
		Solvers: solver.Config{MaxParallel: 3, PathBufferLimit: 64, GraphCacheBudget: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	o := model.Opening{ID: "o99", WallID: "w45", Center: model.Point{X: 4000, Y: 2645}, Width: 180, Active: true}
	report, err := s.Apply(ctx, model.Edit{Kind: model.EditAddPortal, Portal: &o})
	if err != nil {
		t.Fatal(err)
	}
	reused := 0
	for _, r := range report.Invalidations {
		if r.Reused {
			reused++
		}
	}
	if reused == 0 {
		t.Fatal("expected at least one room to reuse cached result")
	}
}
