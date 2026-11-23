package workbench

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestGeneratedSceneTruthCalibration(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultProcessConfig()
	result, err := RenderScene(scene, cfg, PathFull)
	if err != nil {
		t.Fatal(err)
	}
	maxErr := 0.0
	for _, patch := range scene.Truth.Patches {
		x := patch.X + patch.Width/2
		y := patch.Y + patch.Height/2
		i := y*scene.Meta.Width + x
		got := [3]float64{
			result.LinearRGB[i*3],
			result.LinearRGB[i*3+1],
			result.LinearRGB[i*3+2],
		}
		for c := 0; c < 3; c++ {
			maxErr = math.Max(maxErr, math.Abs(got[c]-patch.LinearRGB[c]))
		}
		t.Logf("%s truth=%v got=%v", patch.ID, patch.LinearRGB, got)
	}
	peakErr := math.Abs(result.PeakRadiance-scene.Truth.PeakRadiance) / scene.Truth.PeakRadiance
	t.Logf("peak truth=%.6f got=%.6f relerr=%.6f maxPatchErr=%.6f OOG=%d", scene.Truth.PeakRadiance, result.PeakRadiance, peakErr, maxErr, result.OutOfGamut)
	if maxErr > scene.Truth.Tolerance.PatchRGB {
		t.Fatalf("patch error %.6f exceeds %.6f", maxErr, scene.Truth.Tolerance.PatchRGB)
	}
	if peakErr > scene.Truth.Tolerance.PeakRadianceRel {
		t.Fatalf("peak error %.6f exceeds %.6f", peakErr, scene.Truth.Tolerance.PeakRadianceRel)
	}
}

func TestPathConsistency(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultProcessConfig()
	full, err := RenderScene(scene, cfg, PathFull)
	if err != nil {
		t.Fatal(err)
	}
	tiled, err := RenderScene(scene, cfg, PathTiled)
	if err != nil {
		t.Fatal(err)
	}
	report, err := CompareRenderResults(full, tiled, tiled.Scale.TileWidth, tiled.Scale.TileHeight, scene.Truth.Tolerance)
	if err != nil {
		t.Fatalf("compare failed: %v report=%+v", err, report)
	}
	if !report.Consistent {
		t.Fatalf("paths inconsistent: %+v", report)
	}
	if tiled.Scale.CoarseSamples == 0 {
		t.Fatal("tiled path did not execute its downsampled coarse analysis")
	}
}

func TestExposureWeightsSuppressClippedSamples(t *testing.T) {
	cfg := DefaultProcessConfig()
	weights := exposureWeights([]float64{0, 0.5, 1}, cfg)
	if weights.reliability[0] > 1e-5 || weights.reliability[2] > 1e-5 {
		t.Fatalf("clipped samples were not suppressed: %v", weights.reliability)
	}
	if weights.reliability[1] < 0.99 {
		t.Fatalf("mid-tone reliability is too low: %v", weights.reliability)
	}
	if weights.weight[0]+weights.weight[2] > 1e-4 {
		t.Fatalf("clipped samples retained significant fusion weight: %v", weights.weight)
	}
}

func TestThreadSchedulingDeterminism(t *testing.T) {
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(workers int) *RenderResult {
		cfg := DefaultProcessConfig()
		cfg.MaxWorkers = workers
		engine, err := NewEngine(scene, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer engine.Close()
		if err := engine.Run(ctx); err != nil {
			t.Fatal(err)
		}
		result, err := engine.Result()
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	single := run(1)
	parallel := run(4)
	report, err := CompareRenderResults(single, parallel, 64, 64, scene.Truth.Tolerance)
	if err != nil {
		t.Fatalf("thread scheduling changed pixels: %v (%+v)", err, report)
	}
}

func TestEightBandWideGamutScene(t *testing.T) {
	cfg := DefaultGenerateConfig()
	cfg.Width = 40
	cfg.Height = 32
	cfg.Bands = 8
	cfg.Exposures = 4
	cfg.BandCenters = []float64{410, 455, 495, 535, 575, 615, 655, 695}
	cfg.ExposureTimes = []float64{1.0 / 1000, 1.0 / 250, 1.0 / 60, 1.0 / 12}
	cfg.TargetGamut = "Rec2020"
	scene, err := GenerateScene(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(scene.Bands) != 8 || len(scene.Exposures) != 4 {
		t.Fatalf("unexpected scene shape: %d bands, %d exposures", len(scene.Bands), len(scene.Exposures))
	}
	result, err := RenderScene(scene, DefaultProcessConfig(), PathTiled)
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range scene.Truth.Patches {
		x := patch.X + patch.Width/2
		y := patch.Y + patch.Height/2
		i := (y*result.Width + x) * 3
		for c := 0; c < 3; c++ {
			if math.Abs(result.LinearRGB[i+c]-patch.LinearRGB[c]) > scene.Truth.Tolerance.PatchRGB {
				t.Fatalf("patch %s channel %d outside truth tolerance", patch.ID, c)
			}
		}
	}
}
