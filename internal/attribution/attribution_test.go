package attribution_test

import (
	"errors"
	"testing"

	"qkernel/internal/attribution"
	"qkernel/internal/errs"
	"qkernel/internal/imagex"
	"qkernel/internal/metrics"
)

func metricWithNoise(v float64) *metrics.Report {
	return &metrics.Report{
		BlockSize: 32, Width: 32, Height: 32,
		Blocks: []metrics.Block{{Index: 0, X: 0, Y: 0, Rect: imagex.R(0, 0, 32, 32), NoiseVariance: v}},
	}
}

func TestAmbiguousAttributionReturnsCandidates(t *testing.T) {
	states := []*metrics.Report{
		metricWithNoise(0), metricWithNoise(10), metricWithNoise(20), metricWithNoise(30),
	}
	rep, err := attribution.BuildReport(states, []string{"noise", "noise", "noise"}, attribution.DefaultDeclaredShare)
	if !errs.IsCode(err, errs.EAttr) {
		t.Fatalf("expected E_ATTR, got %v", err)
	}
	if len(rep.Blocks) != 1 || rep.Blocks[0].Status != "ambiguous" {
		t.Fatalf("unexpected report: %+v", rep)
	}
	ke := asError(t, err)
	if ke.BlockX != 0 || ke.BlockY != 0 || len(ke.Candidates) != 3 {
		t.Fatalf("error missing block/candidates: %+v", ke)
	}
}

func TestUniqueStepAttribution(t *testing.T) {
	base := metricWithNoise(1)
	step1 := metricWithNoise(20)
	step2 := metricWithNoise(19)
	rep, err := attribution.BuildReport([]*metrics.Report{base, step1, step2}, []string{"noise", "blur"}, 0.45)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Blocks[0].StepIndex != 1 {
		t.Fatalf("expected first step, got %+v", rep.Blocks[0])
	}
}

func asError(t *testing.T, err error) *errs.Error {
	t.Helper()
	var ke *errs.Error
	if !errors.As(err, &ke) {
		t.Fatalf("expected errs.Error, got %T", err)
	}
	return ke
}
