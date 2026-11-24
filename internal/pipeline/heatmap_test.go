package pipeline_test

import (
	"testing"

	"collectionrecon/internal/pipeline"
)

func TestBuildHeatTilesMatchesStreamedTiles(t *testing.T) {
	region := newRegion()
	engine := pipeline.NewEngine(region, pipeline.DefaultLimits())
	result, err := engine.ComputeZone(2)
	if err != nil {
		t.Fatal(err)
	}
	whole := pipeline.BuildHeatTiles(region, 2, result.Points)
	if len(whole) != len(result.HeatTiles) {
		t.Fatalf("tile count %d != streamed %d", len(whole), len(result.HeatTiles))
	}
	for i := range whole {
		a, b := whole[i], result.HeatTiles[i]
		if a.IX != b.IX || a.IY != b.IY || a.Density != b.Density {
			t.Fatalf("tile %d differs: %+v vs %+v", i, a, b)
		}
	}
}
