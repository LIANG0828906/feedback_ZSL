package raster_test

import (
	"testing"

	"glyphbrush/internal/errs"
	"glyphbrush/internal/geom"
	"glyphbrush/internal/model"
	"glyphbrush/internal/raster"
	"glyphbrush/internal/skeleton"
	"glyphbrush/internal/stroke"
)

func TestGridDerivedFromTruthBounds(t *testing.T) {
	truth := geom.Bounds{MinX: 0, MinY: 0, MaxX: 200, MaxY: 100}
	cfg := raster.Config{Scale: 0.5, GridSize: 1, MaxDimension: 1000}
	grid, err := raster.NewGrid(truth, 20, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if grid.Width != int(grid.WorldBounds.Width()*cfg.Scale) ||
		grid.Height != int(grid.WorldBounds.Height()*cfg.Scale) {
		t.Fatalf("grid dimensions not derived: %dx%d world %.3fx%.3f",
			grid.Width, grid.Height, grid.WorldBounds.Width(), grid.WorldBounds.Height())
	}
	if !grid.WorldBounds.ContainsBounds(truth) {
		t.Fatal("grid does not contain truth bounds")
	}
}

func TestRasterizeAndComposite(t *testing.T) {
	truth := geom.Bounds{MinX: -50, MinY: -50, MaxX: 150, MaxY: 150}
	grid, err := raster.NewGrid(truth, 10, raster.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	poly := []geom.Point{{X: 0, Y: 0}, {X: 100, Y: 0}, {X: 100, Y: 100}, {X: 0, Y: 100}}
	m1, err := raster.RasterizeBand(grid, poly)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := raster.RasterizeBand(grid, poly)
	if err != nil {
		t.Fatal(err)
	}
	if got := countNonZero(m1); got < 90*90 {
		t.Fatalf("coverage too small: %d", got)
	}
	comp := raster.Composite([]raster.Mask{m1, m2})
	nonzero := 0
	for _, v := range comp.Data {
		if v > 0 {
			nonzero++
		}
	}
	if nonzero != countNonZero(m1) {
		t.Fatalf("composite footprint differs: %d vs %d", nonzero, countNonZero(m1))
	}
	found := false
	for _, v := range comp.Data {
		if v == 170 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected two-band overlap gray value 170")
	}
}

func TestDensityAndErrors(t *testing.T) {
	truth := geom.Bounds{MinX: -20, MinY: -20, MaxX: 20, MaxY: 20}
	grid, err := raster.NewGrid(truth, 4, raster.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	mask, err := raster.RasterizeBand(grid, []geom.Point{
		{X: -10, Y: -10}, {X: 10, Y: -10}, {X: 10, Y: 10}, {X: -10, Y: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	params := stroke.DefaultParams()
	params.Width = 4
	points := []skeleton.Point{
		{GlyphID: "g", ContourIndex: 0, CurveIndex: 0, ArcLength: 0, Position: geom.Point{X: 0, Y: 0}},
	}
	boundary := []stroke.BoundarySample{{ArcLength: 0, Center: geom.Point{X: 0, Y: 0}}}
	density, err := raster.ComputeDensities(grid, mask, points, boundary, params.Width, model.DensityRange{MinPPM: 0, MaxPPM: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(density) != 1 || density[0].DensityPPM < 900 {
		t.Fatalf("unexpected density: %+v", density)
	}
	_, err = raster.ComputeDensities(grid, mask, points, boundary, params.Width, model.DensityRange{MinPPM: 1001, MaxPPM: 1001})
	if !errs.IsCode(err, errs.CodeDens) {
		t.Fatalf("expected E_DENS, got %v", err)
	}
	cfg := raster.Config{Scale: 100, GridSize: 1, MaxDimension: 10}
	_, err = raster.NewGrid(truth, 4, cfg)
	if !errs.IsCode(err, errs.CodeGrid) {
		t.Fatalf("expected E_GRID, got %v", err)
	}
}

func TestFirstDifference(t *testing.T) {
	a := raster.NewMask(3, 2)
	b := raster.NewMask(3, 2)
	b.Data[4] = 9
	x, y, found := raster.FirstDifference(a, b)
	if !found || x != 1 || y != 1 {
		t.Fatalf("got (%d,%d,%v), want (1,1,true)", x, y, found)
	}
}

func countNonZero(m raster.Mask) int {
	count := 0
	for _, v := range m.Data {
		if v != 0 {
			count++
		}
	}
	return count
}
