package runner

import (
	"testing"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
	"edgerefine/internal/sample"
)

func testBundle(t *testing.T, spec sample.Spec) *sample.Bundle {
	t.Helper()
	root := t.TempDir()
	if _, err := sample.Generate(root, spec); err != nil {
		t.Fatal(err)
	}
	b, err := sample.LoadBundle(root + "/" + spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParallelSchedulingProducesSamePixelsAndSequence(t *testing.T) {
	b := testBundle(t, sample.DefaultSpecs()[0])
	sequential, err := Run(b, Options{Workers: 1, Params: b.Manifest.Params, StrictAccuracy: true})
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := Run(b, Options{Workers: 8, Params: b.Manifest.Params, StrictAccuracy: true})
	if err != nil {
		t.Fatal(err)
	}
	if sequential.MaskHash != parallel.MaskHash || sequential.DeviationHash != parallel.DeviationHash {
		t.Fatal("global hashes differ across scheduling order")
	}
	if len(sequential.Tiles) != len(parallel.Tiles) {
		t.Fatal("tile count differs")
	}
	for i := range sequential.Tiles {
		a := sequential.Tiles[i]
		c := parallel.Tiles[i]
		if string(a.RefinedMask) != string(c.RefinedMask) || a.FinalResidual != c.FinalResidual {
			t.Fatalf("tile %v differs under parallel scheduling", a.Tile)
		}
	}
	if len(sequential.CommitSequence) == 0 || len(sequential.CommitSequence) != len(parallel.CommitSequence) {
		t.Fatal("commit sequence length differs")
	}
	for i := range sequential.CommitSequence {
		if sequential.CommitSequence[i] != parallel.CommitSequence[i] {
			t.Fatalf("commit sequence differs at %d", i)
		}
	}
}

func TestAllDefaultSamplesMeetEdgeTolerance(t *testing.T) {
	root := t.TempDir()
	ms, err := sample.GenerateAll(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		b, err := sample.LoadBundle(root + "/" + m.SampleID)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Run(b, Options{Workers: 4, Params: m.Params, StrictAccuracy: true})
		if err != nil {
			t.Fatalf("%s: %v", m.SampleID, err)
		}
		if !rep.Accuracy.Pass {
			t.Fatalf("%s accuracy failed: %+v", m.SampleID, rep.Accuracy.OutOfTolerance)
		}
		if rep.Reference == nil || rep.Reference.IoU < 0.90 {
			t.Fatalf("%s reference mask too different: %+v", m.SampleID, rep.Reference)
		}
	}
}

func TestIterationBudgetExhaustedReturnsEIterWithTile(t *testing.T) {
	b := testBundle(t, sample.DefaultSpecs()[2])
	p := b.Manifest.Params
	p.IterBudget = 1
	p.ConvergeTolerance = 1e-9
	rep, err := Run(b, Options{Workers: 1, Params: p})
	if err == nil {
		t.Fatal("expected E_ITER")
	}
	ee, ok := errs.As(err)
	if !ok || ee.Code != errs.EIter {
		t.Fatalf("expected E_ITER, got %v", err)
	}
	if ee.Violation.Tile == [2]int{} && rep == nil {
		t.Fatal("error should identify tile")
	}
}

func TestPreviewAndExactDivergenceGate(t *testing.T) {
	b := testBundle(t, sample.DefaultSpecs()[0])
	preview, err := RunPreview(b, b.Manifest.Params, nil, 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Report.Preview.WithinLimit {
		t.Fatalf("preview delay %dms exceeds %dms", preview.Report.Preview.DelayMS, b.Manifest.Params.PreviewDelayMS)
	}
	exact, err := Run(b, Options{Workers: 2, Params: b.Manifest.Params, Snapshots: preview.Snapshots})
	if err != nil {
		t.Fatal(err)
	}
	div, err := ComparePreviewExact(preview.Report, exact, b.Manifest.Params)
	if err != nil {
		t.Fatalf("expected default divergence tolerances to pass: %v (%+v)", err, div)
	}
	strict := b.Manifest.Params
	strict.PreviewMedianTol = -1
	_, err = ComparePreviewExact(preview.Report, exact, strict)
	if ee, ok := errs.As(err); !ok || ee.Code != errs.EDiverge {
		t.Fatalf("expected E_DIVERGE, got %v", err)
	}
}

func TestRadiusBiasDirectionIsMonotonic(t *testing.T) {
	b := testBundle(t, sample.DefaultSpecs()[0])
	var signs []float64
	for r := 0; r <= 4; r++ {
		p := b.Manifest.Params
		p.Radius = r
		rep, err := Run(b, Options{Workers: 1, Params: p})
		if err != nil {
			t.Fatal(err)
		}
		signs = append(signs, rep.Accuracy.SignedMedian)
	}
	for i := 1; i < len(signs); i++ {
		if signs[i] > signs[i-1]+1e-9 {
			t.Fatalf("signed bias not non-increasing at radius %d: %.6f > %.6f", i, signs[i], signs[i-1])
		}
	}
}

var _ = model.DefaultParams
