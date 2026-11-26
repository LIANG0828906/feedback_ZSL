package pipeline

import (
	"encoding/json"
	"testing"

	"msworkbench/internal/domain"
	"msworkbench/internal/e"
	"msworkbench/internal/graph"
	"msworkbench/internal/quant"
	"msworkbench/internal/recon"
	"msworkbench/internal/sample"
)

func TestAdaptiveThresholdInjection(t *testing.T) {
	d := quant.Select(domain.ScaleDense, 80, quant.Thresholds{SparseMaxObservations: 100, MediumMaxObservations: 200})
	if d.Method != quant.MethodExact {
		t.Fatalf("injected sparse threshold should select exact, got %s", d.Method)
	}
	d = quant.Select(domain.ScaleDense, 80, quant.DefaultThresholds())
	if d.Method != quant.MethodLUT {
		t.Fatalf("default dense selection should use LUT, got %s", d.Method)
	}
}

func TestSchedulerPixelBufferParameter(t *testing.T) {
	p := domain.Project{Width: 16, Height: 12, BlockWidth: 4, BlockHeight: 4, Bands: make([]domain.Band, 5)}
	set := sample.MustGenerate()
	_, _, err := computeBlocks(&p, set, nil, domain.ScaleDense, quant.DefaultThresholds(), []domain.BlockCoord{{BX: 0, BY: 0}}, 1, 1, 1)
	ce, ok := e.As(err)
	if !ok || ce.Code != e.Para || ce.Field != "scheduler.pixel_buffer_per_worker" {
		t.Fatalf("expected E_PARA pixel buffer cap, got %#v", err)
	}
}

func TestInvalidGainReturnsEParam(t *testing.T) {
	v, err := RunVerification(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := graph.Build(v.BaseProject, v.Sample, domain.ScaleDense, quant.DefaultThresholds())
	_, err = g.EditGain("B01", 10)
	ce, ok := e.As(err)
	if !ok || ce.Code != e.Para || ce.Field == "" || ce.Detail == "" {
		t.Fatalf("expected E_PARA with field and range, got %#v", err)
	}
}

func TestDriftBeyondBoundReturnsEDrift(t *testing.T) {
	v, err := RunVerification(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exact := v.Sparse.Blocks[0]
	var candidateQ quant.Result
	b, _ := json.Marshal(v.Dense.Blocks[0].Quant)
	if err := json.Unmarshal(b, &candidateQ); err != nil {
		t.Fatal(err)
	}
	band := candidateQ.Bands["B01"]
	idx := 0
	for i, qv := range band.Values {
		if qv < 5000 {
			idx = i
			break
		}
	}
	band.Values[idx] += 500
	if band.Values[idx] > v.BaseProject.QuantScale {
		band.Values[idx] = 9999
	}
	candidateR, err := recon.ReconstructBlock(v.BaseProject, &candidateQ)
	if err != nil {
		t.Fatal(err)
	}
	err = recon.CheckDrift(v.BaseProject, exact.Quant, &candidateQ, exact.Recon, candidateR)
	ce, ok := e.As(err)
	if !ok || ce.Code != e.Drift {
		t.Fatalf("expected E_DRIFT, got %#v", err)
	}
	if ce.Block != exact.Block.String() || ce.Band == "" || ce.Observed <= ce.Bound {
		t.Fatalf("drift diagnostics incomplete: %#v", ce)
	}
}

func TestCompareReportsReturnsEDiffLocation(t *testing.T) {
	v, err := RunVerification(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v.Full)
	var mutated Report
	if err := json.Unmarshal(raw, &mutated); err != nil {
		t.Fatal(err)
	}
	mutated.Blocks[0].Quant.Bands["B01"].Values[0]++
	err = CompareReports(v.Full, &mutated)
	ce, ok := e.As(err)
	if !ok || ce.Code != e.Diff || ce.Block == "" || ce.Band != "B01" {
		t.Fatalf("expected E_DIFF at first block/B01, got %#v", err)
	}
}
