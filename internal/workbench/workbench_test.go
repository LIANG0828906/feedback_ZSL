package workbench

import (
	"os"
	"path/filepath"
	"testing"

	"edgeworkbench/internal/core"
)

func TestPreviewExactVersionLockAndDivergenceDiagnostics(t *testing.T) {
	w := New(t.TempDir())
	state, err := w.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if w.store != nil {
		defer w.store.Close()
	}
	if len(state.Samples) < 2 {
		t.Fatalf("expected local samples, got %d", len(state.Samples))
	}
	params := state.Defaults
	preview := w.Preview(params)
	if preview.Error != nil {
		t.Fatalf("Preview: %v", preview.Error)
	}
	if !raceEnabled && preview.View.ElapsedMS > 100 {
		t.Fatalf("preview exceeded 100ms: %.2f", preview.View.ElapsedMS)
	}
	exact := w.Exact(ExactRequest{Params: params, ExpectedVersion: preview.View.ParamVersion})
	if exact.Error != nil {
		t.Fatalf("Exact: %v", exact.Error)
	}
	if exact.View == nil || exact.View.Status != "complete" {
		t.Fatalf("unexpected exact view: %+v", exact.View)
	}
	if exact.Divergence == nil || !exact.Divergence.Pass {
		t.Fatalf("preview/exact should pass: %+v", exact.Divergence)
	}
	if len(exact.View.Stages) != len(core.StageOrder) {
		t.Fatalf("stage view count=%d", len(exact.View.Stages))
	}

	if _, err := w.LoadSample("rgb-2048-wide"); err != nil {
		t.Fatalf("LoadSample large: %v", err)
	}
	largePreview := w.Preview(params)
	if largePreview.Error != nil {
		t.Fatalf("large Preview: %v", largePreview.Error)
	}
	if !raceEnabled && largePreview.View.ElapsedMS > 100 {
		t.Fatalf("large end-to-end preview exceeded 100ms: %.2f", largePreview.View.ElapsedMS)
	}

	bad := params
	bad.LowThreshold = 200
	bad.HighThreshold = 10
	mismatch := w.Exact(ExactRequest{Params: bad, ExpectedVersion: preview.View.ParamVersion})
	if mismatch.Error == nil || mismatch.Error.Code != core.CodeParam || mismatch.Error.Field != "lowThreshold" {
		t.Fatalf("bad params should return E_PARA lowThreshold: %+v", mismatch.Error)
	}
	wrongVersion := w.Exact(ExactRequest{Params: params, ExpectedVersion: "stale-version"})
	if wrongVersion.Error == nil || wrongVersion.Error.Code != core.CodeState || wrongVersion.Error.Field != "paramVersion" {
		t.Fatalf("stale version should return E_STATE paramVersion: %+v", wrongVersion.Error)
	}

	if _, err := w.LoadSample("rgb-960-noisy"); err != nil {
		t.Fatalf("LoadSample: %v", err)
	}
	preview = w.Preview(params)
	if preview.Error != nil {
		t.Fatalf("second Preview: %v", preview.Error)
	}
	w.tolerance = core.Tolerance{CoverageDeviation: 0, GeometryDeviation: 0, PixelFraction: 0}
	diverged := w.Exact(ExactRequest{Params: params, ExpectedVersion: preview.View.ParamVersion})
	if diverged.Error == nil || diverged.Error.Code != core.CodeDiverge {
		t.Fatalf("tight tolerance should return E_DIVERGE: %+v", diverged.Error)
	}
	for _, key := range []string{"coverageDeviation", "geometryDeviation", "pixelDiffFraction", "firstMismatch"} {
		if _, ok := diverged.Error.Details[key]; !ok {
			t.Fatalf("E_DIVERGE details missing %s: %+v", key, diverged.Error.Details)
		}
	}
	if diverged.View == nil || diverged.View.HeatmapURL == "" {
		t.Fatal("E_DIVERGE should still return a diagnostic view and heatmap")
	}

	export, err := w.ExportDiagnostics()
	if err != nil {
		t.Fatalf("ExportDiagnostics: %v", err)
	}
	if export.FileCount < len(core.StageOrder)+2 {
		t.Fatalf("export file count=%d", export.FileCount)
	}
	if _, err := os.Stat(filepath.Join(export.Path, "diagnostics.json")); err != nil {
		t.Fatalf("diagnostics.json missing: %v", err)
	}
}
