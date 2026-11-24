package pipeline_test

import (
	"testing"

	"cadpipe/internal/cad"
	"cadpipe/internal/pipeline"
	"cadpipe/internal/render"
	"cadpipe/internal/sample"
)

func TestClipProbesMapToStableTiles(t *testing.T) {
	bundle, _ := sample.Generate(sample.DefaultSeed)
	for _, probe := range bundle.Truth.ClipProbes {
		patch := bundle.Scene.Patch(probe.PatchID)
		tileID, ok := render.ProbeTile(bundle.Camera, patch, probe.U, probe.V)
		if !ok {
			t.Fatalf("probe %s did not project", probe.ID)
		}
		tx, ty := bundle.Camera.TileXY(tileID)
		if tx < 0 || ty < 0 || tx >= bundle.Camera.TileCountX() || ty >= bundle.Camera.TileCountY() {
			t.Fatalf("probe %s mapped outside tile grid: %d", probe.ID, tileID)
		}
	}

	p, err := pipeline.New(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes, pipeline.Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.UpdateClip("surface-alpha", cad.ClipBoundary{Loops: [][]cad.Vec2{{
		{U: 0.18, V: 0.18}, {U: 0.82, V: 0.18},
		{U: 0.82, V: 0.64}, {U: 0.64, V: 0.82}, {U: 0.18, V: 0.82},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tiles) != 1 || len(plan.Tiles[0].FlippedProbes) != 1 {
		t.Fatalf("expected one flip tile/probe, got %+v", plan.Tiles)
	}
	if plan.Tiles[0].FlippedProbes[0] != "surface-alpha-upper-right-inside" {
		t.Fatalf("unexpected flip set: %+v", plan.Tiles[0].FlippedProbes)
	}
}
