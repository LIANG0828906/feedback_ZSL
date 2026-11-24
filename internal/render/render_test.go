package render_test

import (
	"errors"
	"path/filepath"
	"testing"

	"strata/internal/formats"
	"strata/internal/generator"
	"strata/internal/model"
	"strata/internal/pipeline"
	"strata/internal/render"
)

func builtMesh(t *testing.T) *model.Mesh {
	t.Helper()
	dir := t.TempDir()
	if _, err := generator.Generate(195, dir); err != nil {
		t.Fatal(err)
	}
	geometry, err := formats.LoadFile(filepath.Join(dir, "scene-ascii-v1.ply"), model.VersionPLYASCII)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.NewBuilder().Build(geometry, pipeline.DefaultConfig(), "exact", 1)
	if err != nil {
		t.Fatal(err)
	}
	return result.Mesh
}

func TestPreviewBudgetAndAutomaticAgreement(t *testing.T) {
	mesh := builtMesh(t)
	camera := render.DefaultCamera()
	clip := render.ClipPlane{Enabled: true, Axis: "z", Value: -0.2, KeepAbove: true}
	preview, err := render.Render(mesh, camera, clip, render.RenderOptions{
		Mode: "preview", SampleRate: 0.35, FlatShade: true, FrameID: "P1", Version: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.LatencyMs >= 50 || !preview.WithinBudget {
		t.Fatalf("preview latency %.3fms is not within 50ms", preview.LatencyMs)
	}
	exact, err := render.Render(mesh, camera, clip, render.RenderOptions{
		Mode: "exact", SampleRate: 1, FlatShade: false, FrameID: "E1", Version: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := render.Compare(preview, exact)
	if err != nil {
		t.Fatal(err)
	}
	if !comparison.Passed || comparison.CoverageDeviation != 0 ||
		comparison.BBoxProjectionDelta != 0 || comparison.NormalHistogramL1 != 0 {
		t.Fatalf("unexpected comparison: %+v", comparison)
	}
}

func TestDivergenceReportsMetricsPixelAndChunk(t *testing.T) {
	mesh := builtMesh(t)
	camera := render.DefaultCamera()
	clip := render.ClipPlane{Axis: "z", KeepAbove: true}
	preview, err := render.Render(mesh, camera, clip, render.RenderOptions{
		Mode: "preview", SampleRate: 0.35, FlatShade: true, FrameID: "P2", Version: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := render.Render(mesh, camera, clip, render.RenderOptions{
		Mode: "exact", SampleRate: 1, FlatShade: false, FrameID: "E2", Version: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range preview.Mask {
		preview.Mask[i] = 0
	}
	comparison, err := render.Compare(preview, exact)
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Code != model.EDIVERGE {
		t.Fatalf("expected E_DIVERGE, got %v", err)
	}
	if comparison == nil || comparison.Passed || comparison.FirstMismatch == nil {
		t.Fatalf("missing divergence details: %+v", comparison)
	}
	if comparison.FirstMismatch.ChunkID == "" {
		t.Fatalf("first mismatch has no chunk id")
	}
	if comparison.CoverageDeviation <= comparison.Tolerances.Coverage {
		t.Fatalf("coverage deviation %.4f did not exceed tolerance", comparison.CoverageDeviation)
	}
}

func TestCameraAndClipValidation(t *testing.T) {
	bad := render.DefaultCamera()
	bad.Distance = 0
	if err := bad.Validate(); err == nil || model.AsAppError(err).Code != model.EPARA {
		t.Fatalf("expected E_PARA camera error, got %v", err)
	}
	clip := render.ClipPlane{Enabled: true, Axis: "q"}
	if err := clip.Validate(); err == nil || model.AsAppError(err).Code != model.EPARA {
		t.Fatalf("expected E_PARA clip error, got %v", err)
	}
}
