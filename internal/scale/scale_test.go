package scale

import (
	"testing"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/model"
)

func TestDecideIsDeterministicAndSwitchesModes(t *testing.T) {
	small, err := Decide(80, 64, 16, 24, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if small.Mode != ModeFine {
		t.Fatalf("tiny mode = %s, want fine", small.Mode)
	}
	large1, err := Decide(480, 360, 160, 24, "auto")
	if err != nil {
		t.Fatal(err)
	}
	large2, err := Decide(480, 360, 160, 24, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if large1.Mode != ModeCandidate || large1.Stride != 14 || large1.BlockDownsample != 2 {
		t.Fatalf("large decision = %+v", large1)
	}
	if large1.Basis != large2.Basis || large1.Stride != large2.Stride || len(large1.SkippedRanges) != len(large2.SkippedRanges) {
		t.Fatal("decision depends on non-input state")
	}
	if len(large1.SkippedRanges) == 0 {
		t.Fatal("candidate-priority decision did not record skipped ranges")
	}
	if len(large1.ScreenedFrames) > large1.SamplingBudget {
		t.Fatalf("screened %d frames exceeds budget %d", len(large1.ScreenedFrames), large1.SamplingBudget)
	}
	tinyBudget, err := Decide(480, 360, 160, 1, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(tinyBudget.ScreenedFrames) > 1 {
		t.Fatalf("budget=1 screened %d frames", len(tinyBudget.ScreenedFrames))
	}
}

func TestScaleInvalidBudget(t *testing.T) {
	_, err := Decide(80, 64, 16, 0, "auto")
	e := apperr.As(err)
	if e == nil || e.Code != apperr.Para || e.Field != "sampling_budget" {
		t.Fatalf("error = %#v", e)
	}
}

func TestIntervalAgreement(t *testing.T) {
	if err := CheckIntervalAgreement(model.Interval{Start: 10, End: 20}, model.Interval{Start: 11, End: 19}, 1); err != nil {
		t.Fatal(err)
	}
	err := CheckIntervalAgreement(model.Interval{Start: 10, End: 20}, model.Interval{Start: 14, End: 20}, 1)
	e := apperr.As(err)
	if e == nil || e.Code != apperr.Scale || e.Left == nil || e.Right == nil {
		t.Fatalf("expected E_SCALE with both sides, got %#v", e)
	}
}
