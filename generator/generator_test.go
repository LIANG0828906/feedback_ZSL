package generator_test

import (
	"context"
	"testing"

	"terrainbake/bake"
	"terrainbake/generator"
	"terrainbake/model"
)

func TestFixedScaleErrorBoundIsMonotone(t *testing.T) {
	scales := []int64{100, 1000, 10000, 100000}
	var bounds []float64
	for _, scale := range scales {
		bounds = append(bounds, bake.FixedScaleErrorBound(scale))
	}
	for i := 1; i < len(bounds); i++ {
		if bounds[i] > bounds[i-1] {
			t.Fatalf("error bound increased: %v", bounds)
		}
	}
}

func TestActualIrradianceErrorDecreasesWithScale(t *testing.T) {
	scales := []int64{1000, 10000, 100000}
	var maxErrors []float64
	for _, scale := range scales {
		cfg := generator.DefaultConfig(5150)
		cfg.FixedScale = scale
		bundle, err := generator.GenerateWithConfig(cfg)
		if err != nil {
			t.Fatalf("generate scale %d: %v", scale, err)
		}
		engine, _ := bake.NewEngine(&bundle.Project, nil)
		result, err := engine.BakeFull(context.Background(), bake.DefaultScheduleOptions(bundle.Project.Params))
		if err != nil {
			t.Fatalf("bake scale %d: %v", scale, err)
		}
		report, err := bake.CompareTruth(result, bundle.Truth, 0.01)
		if err != nil {
			t.Fatalf("truth scale %d: %v", scale, err)
		}
		maxErrors = append(maxErrors, report.MaxIrradianceError)
		t.Logf("fixed_scale=%d max_irradiance_error=%.12g", scale, report.MaxIrradianceError)
	}
	for i := 1; i < len(maxErrors); i++ {
		if maxErrors[i] > maxErrors[i-1]+1e-12 {
			t.Fatalf("max irradiance error increased with scale: %v", maxErrors)
		}
	}
}

func TestToleranceAcceptanceCountIsMonotone(t *testing.T) {
	bundle, err := generator.Generate(5150)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	engine, _ := bake.NewEngine(&bundle.Project, nil)
	result, err := engine.BakeFull(context.Background(), bake.DefaultScheduleOptions(bundle.Project.Params))
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	tolerances := []float64{0.1, 0.01, 0.001, 0.0001}
	previous := -1
	for _, tolerance := range tolerances {
		report := bake.AnalyzeTruth(result, bundle.Truth, tolerance)
		if report.ShadowMismatches != 0 {
			t.Fatalf("shadow mismatch at tolerance %g", tolerance)
		}
		if previous >= 0 && report.WithinTolerance > previous {
			t.Fatalf("accepted point count increased as tolerance tightened: %d -> %d", previous, report.WithinTolerance)
		}
		t.Logf("tolerance=%.6g within_tolerance=%d over_tolerance=%d max_irr=%.12g",
			tolerance, report.WithinTolerance, report.OverTolerance, report.MaxIrradianceError)
		previous = report.WithinTolerance
	}
}

func TestSkyVisibilityValidationContext(t *testing.T) {
	params := model.BakeParams{FixedScale: 10000, MaxChannelFixed: 640000}
	vertex := model.VertexPoint{X: 2, Z: 3}
	world := model.Vec3{X: 4, Y: 5, Z: 6}
	for _, value := range []float64{-0.01, 1.01} {
		err := bake.ValidateSkyVisibility(value, world, vertex, params)
		coded, ok := model.AsCoded(err)
		if !ok || coded.Code != model.CodeValue || coded.Channel != "visibility" ||
			coded.Point == nil || *coded.Point != vertex || coded.World == nil {
			t.Fatalf("bad E_VAL context for %g: %+v", value, err)
		}
	}
}

func TestSeededTruthStability(t *testing.T) {
	seeds := []int64{0, 1, 7, 42, 77, 512, 991, 5150, 424242}
	for _, seed := range seeds {
		bundle, err := generator.Generate(seed)
		if err != nil {
			t.Fatalf("seed %d generate: %v", seed, err)
		}
		engine, err := bake.NewEngine(&bundle.Project, nil)
		if err != nil {
			t.Fatalf("seed %d engine: %v", seed, err)
		}
		result, err := engine.BakeFull(context.Background(), bake.DefaultScheduleOptions(bundle.Project.Params))
		if err != nil {
			t.Fatalf("seed %d bake: %v", seed, err)
		}
		report, err := bake.CompareTruth(result, bundle.Truth, bundle.Project.Params.IrradianceTolerance)
		if err != nil {
			t.Fatalf("seed %d truth: %v report=%+v", seed, err, report)
		}
		if _, err := bake.AnalyzeSeams(result, engine.Terrain()); err != nil {
			t.Fatalf("seed %d seam: %v", seed, err)
		}
	}
}
