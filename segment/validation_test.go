package segment

import (
	"testing"

	"iconflow/errs"
	"iconflow/model"
)

func TestThresholdBoundsError(t *testing.T) {
	gradient := &GradientResult{
		Coord:     model.BlockCoord{ImageID: "img", TX: 0, TY: 0, Width: 1, Height: 1},
		Magnitude: []int{10},
	}
	_, err := SelectThreshold(gradient, model.Rules{
		QuantilePermyriad: 10000, MinContrast: 100,
		ThresholdMin: 0, ThresholdMax: 50,
	})
	if err == nil || err.Code != errs.EThr {
		t.Fatalf("expected E_THR, got %+v", err)
	}
	if err.Details["measured_quantile"] != 10 {
		t.Fatalf("missing measured quantile: %+v", err.Details)
	}
}

func TestRegionAndTopologyErrors(t *testing.T) {
	coord := model.BlockCoord{ImageID: "img", Width: 2, Height: 1}
	stats := &BlockStats{Coord: coord, Components: []Component{{Area: 10}}}
	truth := &model.BlockTruth{Components: []model.ComponentTruth{{Area: 5}, {Area: 5}}}
	err := ValidateBlock(stats, truth, model.Tolerances{})
	if err == nil || err.Code != errs.ERegion {
		t.Fatalf("expected E_REGION, got %+v", err)
	}

	stats = &BlockStats{Coord: coord, Components: []Component{{
		Area: 10, Centroid: model.Centroid{XMilli: 1000, YMilli: 1000},
	}}}
	truth = &model.BlockTruth{Components: []model.ComponentTruth{{
		Area: 10, Centroid: model.Centroid{XMilli: 9000, YMilli: 1000},
	}}}
	err = ValidateBlock(stats, truth, model.Tolerances{AreaAbs: 1, CentroidMilliAbs: 100})
	if err == nil || err.Code != errs.ETop {
		t.Fatalf("expected E_TOP, got %+v", err)
	}
	if len(err.Issues) != 1 {
		t.Fatalf("expected listed topology issue, got %+v", err.Issues)
	}
}
