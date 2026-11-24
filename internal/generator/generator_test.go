package generator_test

import (
	"testing"

	"collectionrecon/internal/generator"
	"collectionrecon/internal/geom"
)

func TestDeterministicAcrossRuns(t *testing.T) {
	a := generator.Generate(generator.DefaultConfig())
	b := generator.Generate(generator.DefaultConfig())
	if len(a.Truth) != len(b.Truth) {
		t.Fatal("truth point count differs across runs")
	}
	for i := range a.Truth {
		if a.Truth[i].Coord != b.Truth[i].Coord {
			t.Fatalf("truth point %d differs: %+v vs %+v", i, a.Truth[i], b.Truth[i])
		}
	}
	if len(a.Observations) != len(b.Observations) {
		t.Fatal("observation count differs")
	}
	for i := range a.Observations {
		if a.Observations[i].Density != b.Observations[i].Density {
			t.Fatalf("observation %d differs", i)
		}
	}
}

func TestTruthInsideRegionAndOutOfNoCollect(t *testing.T) {
	region := generator.Generate(generator.DefaultConfig())
	for _, tp := range region.Truth {
		if !geom.PointInPolygon(tp.Coord, region.Boundary) {
			t.Fatalf("truth point %+v outside boundary", tp)
		}
		for _, nz := range region.NoCollect {
			if nz.ZoneID == tp.ZoneID && geom.PointInRect(tp.Coord, nz.Bounds) {
				t.Fatalf("truth point %+v inside no-collect %d", tp, nz.ID)
			}
		}
	}
}

func TestRasterCoversBBox(t *testing.T) {
	region := generator.Generate(generator.DefaultConfig())
	if err := geom.RasterCompatible(region.BBox, region.RasterCell,
		region.RasterNX, region.RasterNY); err != nil {
		t.Fatalf("raster incompatible: %v", err)
	}
	if len(region.Observations) == 0 {
		t.Fatal("expected observations")
	}
}
