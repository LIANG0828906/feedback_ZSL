package fit_test

import (
	"testing"

	"prosodyalign/internal/envelope"
	"prosodyalign/internal/errs"
	"prosodyalign/internal/fit"
	"prosodyalign/internal/generator"
)

func TestGeneratedWaterfillCapAndRedistribution(t *testing.T) {
	suite, err := generator.GenerateSuite(generator.FixedSeed, 1)
	if err != nil {
		t.Fatal(err)
	}
	d := suite.Dialogs[0]
	frames, _, err := envelope.Extract(d.Project, nil)
	if err != nil {
		t.Fatal(err)
	}
	accent, err := envelope.LocateAccent(d.Project, frames, d.Project.AccentToleranceSamples)
	if err != nil {
		t.Fatal(err)
	}
	params := d.Project.FitParameters()
	result, err := fit.Allocate(d.Project, params, accent)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Feasible {
		t.Fatalf("initial fit infeasible: shortfall=%d", result.Shortfall)
	}
	wantAdded := map[int64]int64{1: 1600, 3: 1200, 5: 800}
	for idx, want := range wantAdded {
		if got := result.Segments[idx].AddedSamples; got != want {
			t.Fatalf("p%d added=%d want=%d", idx, got, want)
		}
	}
	if !result.Segments[3].CapTriggered || result.Segments[1].CapTriggered {
		t.Fatalf("cap flags wrong: p1=%v p3=%v", result.Segments[1].CapTriggered, result.Segments[3].CapTriggered)
	}

	params.TargetDurationSamples += 1200
	params.DefaultMaxStretch = 2000
	result2, err := fit.Allocate(d.Project, params, accent)
	if err != nil {
		t.Fatal(err)
	}
	if result2.Segments[1].AddedSamples != 2000 || !result2.Segments[1].CapTriggered {
		t.Fatalf("redistribution p1=%+v", result2.Segments[1])
	}
	if result2.Segments[3].AddedSamples != 1200 || !result2.Segments[3].CapTriggered {
		t.Fatalf("bound p3 changed: %+v", result2.Segments[3])
	}
	if result2.Segments[5].AddedSamples != 1600 {
		t.Fatalf("redistributed p5=%d want1600", result2.Segments[5].AddedSamples)
	}
}

func TestAccentToleranceFailureHasBothMeasuredPeaks(t *testing.T) {
	suite, _ := generator.GenerateSuite(generator.FixedSeed, 1)
	p := suite.Dialogs[0].Project
	frames, _, err := envelope.Extract(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	accent, locateErr := envelope.LocateAccent(p, frames, 0)
	if locateErr == nil {
		t.Fatal("expected zero tolerance to fail because PCM peak is not exactly F0 point")
	}
	ce, ok := errs.As(locateErr)
	if !ok || ce.Detail.Code != errs.EFit || ce.Detail.PhonemeIndex == nil {
		t.Fatalf("expected E_FIT with phoneme, got %+v", locateErr)
	}
	if ce.Detail.Context["left_measured"] == nil || ce.Detail.Context["right_measured"] == nil {
		t.Fatalf("missing both measured values: %+v", ce.Detail.Context)
	}
	params := p.FitParameters()
	params.AccentTolerance = 0
	_, err = fit.Allocate(p, params, accent)
	ce, ok = errs.As(err)
	if !ok || ce.Detail.Code != errs.EFit {
		t.Fatalf("fit should propagate E_FIT, got %v", err)
	}
}

func TestUpperBoundExhaustionReturnsFitError(t *testing.T) {
	suite, _ := generator.GenerateSuite(generator.FixedSeed, 1)
	p := suite.Dialogs[0].Project
	params := p.FitParameters()
	params.TargetDurationSamples += 100000
	params.DefaultMaxStretch = 1
	_, err := fit.Allocate(p, params, nil)
	ce, ok := errs.As(err)
	if !ok || ce.Detail.Code != errs.EFit {
		t.Fatalf("expected E_FIT shortfall, got %v", err)
	}
}
