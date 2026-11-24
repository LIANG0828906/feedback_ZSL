//go:build wails

package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAppDiagnosticOutputs(t *testing.T) {
	app := NewApp()
	app.startup(context.Background())
	dir := t.TempDir()
	state, err := app.Initialize(filepath.Join(dir, "sample"), filepath.Join(dir, "ui", "snapshots.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Frames) != 5 {
		t.Fatalf("expected 5 frames, got %d", len(state.Frames))
	}
	view, err := app.Frame(0)
	if err != nil {
		t.Fatal(err)
	}
	if view.Width <= 0 || view.Height <= 0 {
		t.Fatalf("invalid viewport %dx%d", view.Width, view.Height)
	}
	if len(view.RGBA) != view.Width*view.Height*4 {
		t.Fatalf("bitmap channel count mismatch")
	}
	if len(view.Lines) != 2 {
		t.Fatalf("expected contour overlays for both paths")
	}
	if len(view.Heatmap) != len(view.RGBA) {
		t.Fatalf("heatmap channel count mismatch")
	}
	adjusted, err := app.AdjustFrame(0, 128, 10)
	if err != nil {
		t.Fatal(err)
	}
	if adjusted.Quality.Tolerance != 128 || adjusted.Quality.PrecisionBits != 10 {
		t.Fatalf("incremental quality was not applied: %+v", adjusted.Quality)
	}
	retried, err := app.RetryFrame(0)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Index != adjusted.Index || retried.Height != adjusted.Height {
		t.Fatalf("retry changed frame identity")
	}
	status := app.Status()
	if status.FrameCount != 5 || len(status.StateMatrix) == 0 {
		t.Fatalf("diagnostic status is incomplete: %+v", status)
	}
}
