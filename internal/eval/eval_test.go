package eval_test

import (
	"testing"

	"semanticshading/internal/eval"
	"semanticshading/internal/model"
	"semanticshading/internal/sample"
)

func TestRegionMetricsMatchGeneratorTruth(t *testing.T) {
	s, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	records := make([]model.BlockRecord, 0, s.Manifest.BlockCount())
	for i := 0; i < s.Manifest.BlockCount(); i++ {
		records = append(records, eval.EvaluateBlock(s, i, s.Manifest.BandWidth))
	}
	regions := eval.RegionMetricsFromRecords(s.Manifest, records)
	if len(regions) != len(s.Truth.Regions) {
		t.Fatalf("region count = %d, want %d", len(regions), len(s.Truth.Regions))
	}
	var totalArea int64
	for _, region := range regions {
		totalArea += region.Area
		truth, ok := s.Truth.Region(region.Label)
		if !ok {
			t.Fatalf("missing truth for label %d", region.Label)
		}
		if region.Area != int64(truth.Area) {
			t.Fatalf("label %d area = %d, want %d", region.Label, region.Area, truth.Area)
		}
		if delta(region.MeanDistanceQ8, truth.ExpectedMeanQ8) > s.Truth.Tolerance.MeanAbsQ8 {
			t.Fatalf("label %d mean delta too large: %d vs %d", region.Label, region.MeanDistanceQ8, truth.ExpectedMeanQ8)
		}
		if delta(region.StdDistanceQ8, truth.ExpectedStdQ8) > s.Truth.Tolerance.StdAbsQ8 {
			t.Fatalf("label %d std delta too large: %d vs %d", region.Label, region.StdDistanceQ8, truth.ExpectedStdQ8)
		}
		if delta(region.BoundaryJumpQ8, truth.ExpectedBoundaryQ8) > s.Truth.Tolerance.BoundaryAbsQ8 {
			t.Fatalf("label %d boundary delta too large: %d vs %d", region.Label, region.BoundaryJumpQ8, truth.ExpectedBoundaryQ8)
		}
	}
	if totalArea != int64(s.Manifest.Width*s.Manifest.Height) {
		t.Fatalf("counted area = %d, want every pixel once (%d)", totalArea, s.Manifest.Width*s.Manifest.Height)
	}
}

func TestWiderBandDoesNotIncreaseBoundaryDeviation(t *testing.T) {
	s, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	bands := []int{0, 1, 2, 3}
	previous := int64(-1)
	for _, band := range bands {
		records := make([]model.BlockRecord, 0, s.Manifest.BlockCount())
		missing := 0
		for i := 0; i < s.Manifest.BlockCount(); i++ {
			record := eval.EvaluateBlock(s, i, band)
			missing += record.MissingEdges
			records = append(records, record)
		}
		regions := eval.RegionMetricsFromRecords(s.Manifest, records)
		var totalBoundaryDelta int64
		for _, region := range regions {
			truth, _ := s.Truth.Region(region.Label)
			totalBoundaryDelta += delta(region.BoundaryJumpQ8, truth.ExpectedBoundaryQ8)
		}
		t.Logf("band=%d missing=%d boundary_delta=%d", band, missing, totalBoundaryDelta)
		if previous >= 0 && totalBoundaryDelta > previous {
			t.Fatalf("boundary deviation increased at band %d: %d > previous %d", band, totalBoundaryDelta, previous)
		}
		previous = totalBoundaryDelta
	}
}

func TestTighterThresholdMonotonic(t *testing.T) {
	s, _ := sample.Generate(sample.DefaultConfig())
	records := make([]model.BlockRecord, 0, s.Manifest.BlockCount())
	for i := 0; i < s.Manifest.BlockCount(); i++ {
		records = append(records, eval.EvaluateBlock(s, i, s.Manifest.BandWidth))
	}
	regions := eval.RegionMetricsFromRecords(s.Manifest, records)
	thresholds := []int64{60000, 30000, 16000, 10000, 1000}
	previousAccepted := len(regions) + 1
	for _, threshold := range thresholds {
		judgments := eval.Judge(regions, threshold)
		accepted := len(eval.AcceptedLabels(judgments))
		if accepted > previousAccepted {
			t.Fatalf("accepted count increased while threshold tightened: %d > %d at %d", accepted, previousAccepted, threshold)
		}
		previousAccepted = accepted
	}
}

func delta(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}
