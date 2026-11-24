package verify_test

import (
	"context"
	"testing"

	"occlusionlab/internal/generator"
	"occlusionlab/internal/model"
	"occlusionlab/internal/verify"
)

func TestFullAcceptanceReport(t *testing.T) {
	project, err := generator.Generate(generator.DefaultConfig(1042))
	if err != nil {
		t.Fatal(err)
	}
	params := model.Params{
		Quantization: project.Meta.Precision, RasterResolution: 128,
		MaxWorkers: 2, RasterBufferCells: 128 * 128, MaxResident: 15,
		LoadCostBudget: 1000, TolerancePxQ: project.Meta.TolerancePxQ, Version: 1,
	}
	report, appErr := verify.Run(context.Background(), project, params, "")
	if appErr != nil {
		t.Fatal(appErr)
	}
	if !report.Passed {
		t.Fatalf("acceptance report failed: %+v", report.Checks)
	}
	if report.Projection.KnownCount == 0 ||
		report.Projection.WithinTolerance != report.Projection.KnownCount {
		t.Fatalf("unexpected projection report: %+v", report.Projection)
	}
	if report.Projection.BoundViolations != 0 {
		t.Fatal("projection bound did not cover observations")
	}
	if !report.Projection.MonotonicTightening {
		t.Fatal("tolerance tightening is not monotonic")
	}
	if report.Visibility.Conflicts != 0 {
		t.Fatalf("visibility conflicts: %+v", report.Visibility.Mismatches)
	}
	if !report.ThreePaths.Equal || !report.ThreePaths.CheckpointVerified {
		t.Fatalf("three paths failed: %+v", report.ThreePaths)
	}
	if report.ThreePaths.Paths[1].ResumedFrom != 2 {
		t.Fatalf("resume path did not start from 2 committed windows: %+v", report.ThreePaths.Paths[1])
	}
}
