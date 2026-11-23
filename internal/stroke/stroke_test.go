package stroke_test

import (
	"math"
	"testing"

	"glyphbrush/internal/errs"
	"glyphbrush/internal/geom"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/samplegen"
	"glyphbrush/internal/stroke"
)

func loadBundle(t *testing.T) *loader.Bundle {
	t.Helper()
	dir := t.TempDir()
	if _, _, err := samplegen.Write(dir, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	bundle, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestBandIsSimpleAndBounded(t *testing.T) {
	bundle := loadBundle(t)
	glyph := bundle.Glyphs[0]
	run := glyph.Skeletons[0].Runs[0]
	points := make([]geom.Point, len(run.Points))
	for i, p := range run.Points {
		points[i] = p.Position
	}
	params := stroke.DefaultParams()
	band, err := stroke.Build(points, params, glyph.Glyph.Bounds.F(), 100000)
	if err != nil {
		t.Fatal(err)
	}
	if band.Area <= 0 {
		t.Fatal("non-positive band area")
	}
	if self, at := geom.PolygonSelfIntersects(band.Polygon); self {
		t.Fatalf("band self-intersects at %+v", at)
	}
	if !glyph.Glyph.Bounds.F().Expand(params.Width/2*1.35 + 3).ContainsBounds(band.Bounds) {
		t.Fatal("band escaped expanded glyph bounds")
	}
}

func TestBandAreaGrowsWithWidth(t *testing.T) {
	bundle := loadBundle(t)
	glyph := bundle.Glyphs[0]
	run := glyph.Skeletons[0].Runs[0]
	points := make([]geom.Point, len(run.Points))
	for i, p := range run.Points {
		points[i] = p.Position
	}
	params := stroke.DefaultParams()
	var previous float64
	for _, width := range []float64{12, 18, 28, 40, 56} {
		params.Width = width
		band, err := stroke.Build(points, params, glyph.Glyph.Bounds.F(), 100000)
		if err != nil {
			t.Fatalf("width %.1f: %v", width, err)
		}
		if previous > 0 && band.Area+1e-6 < previous {
			t.Fatalf("area not monotonic at width %.1f: %.3f < %.3f", width, band.Area, previous)
		}
		previous = band.Area
	}
}

func TestSpacingTightensWithCurvature(t *testing.T) {
	params := stroke.DefaultParams()
	straight := params.EffectiveSpacing(0)
	curved := params.EffectiveSpacing(1 / 30.0)
	if !(curved < straight) {
		t.Fatalf("spacing did not tighten: straight=%f curved=%f", straight, curved)
	}
}

func TestInvalidParamsReportField(t *testing.T) {
	params := stroke.DefaultParams()
	params.Width = 0
	_, err := stroke.Build([]geom.Point{{X: 0, Y: 0}, {X: 10, Y: 0}}, params, geom.Bounds{MinX: -20, MinY: -20, MaxX: 30, MaxY: 20}, 1000)
	if !errs.IsCode(err, errs.CodeParam) {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	e, _ := errs.As(err)
	if e.Param != "width" {
		t.Fatalf("expected width field, got %q", e.Param)
	}
	params = stroke.DefaultParams()
	params.Spacing = 0
	_, err = stroke.Build([]geom.Point{{X: 0, Y: 0}, {X: 10, Y: 0}}, params, geom.Bounds{MinX: -20, MinY: -20, MaxX: 30, MaxY: 20}, 1000)
	e, _ = errs.As(err)
	if !errs.IsCode(err, errs.CodeParam) || e.Param != "spacing" {
		t.Fatalf("expected spacing E_PARA, got %v", err)
	}
}

func TestZeroLengthSkeleton(t *testing.T) {
	params := stroke.DefaultParams()
	_, err := stroke.Build([]geom.Point{{X: 1, Y: 1}, {X: 1, Y: 1}}, params, geom.Bounds{MinX: -20, MinY: -20, MaxX: 30, MaxY: 20}, 1000)
	if !errs.IsCode(err, errs.CodeZero) {
		t.Fatalf("expected E_ZLEN, got %v", err)
	}
}

func TestSelfIntersectingSkeletonReturnsESelf(t *testing.T) {
	params := stroke.DefaultParams()
	points := []geom.Point{{X: 0, Y: 0}, {X: 100, Y: 100}, {X: 0, Y: 100}, {X: 100, Y: 0}}
	_, err := stroke.Build(points, params, geom.Bounds{MinX: -100, MinY: -100, MaxX: 200, MaxY: 200}, 1000)
	if !errs.IsCode(err, errs.CodeSelf) {
		t.Fatalf("expected E_SELF, got %v", err)
	}
	e, _ := errs.As(err)
	if e.Param != "controlPoints" {
		t.Fatalf("expected controlPoints parameter, got %q", e.Param)
	}
}

func TestPolygonBufferLimit(t *testing.T) {
	params := stroke.DefaultParams()
	_, err := stroke.Build([]geom.Point{{X: 0, Y: 0}, {X: 100, Y: 0}}, params,
		geom.Bounds{MinX: -100, MinY: -100, MaxX: 200, MaxY: 100}, 4)
	if !errs.IsCode(err, errs.CodeParam) {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	e, _ := errs.As(err)
	if e.Param != "maxPolygonPoints" {
		t.Fatalf("unexpected parameter %q", e.Param)
	}
}

func TestArcBetweenGeometry(t *testing.T) {
	// Keep a sanity check on a known straight band area. A 100x10 flat-cap
	// rectangle has area exactly 1000.
	params := stroke.Params{Width: 10, Spacing: 1, Join: stroke.JoinMiter, Tip: stroke.TipFlat, CurvatureFactor: 0}
	band, err := stroke.Build([]geom.Point{{X: 0, Y: 0}, {X: 100, Y: 0}}, params, geom.Bounds{MinX: -100, MinY: -100, MaxX: 200, MaxY: 100}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(band.Area-1000) > 1e-6 {
		t.Fatalf("flat rectangle area = %.6f, want 1000", band.Area)
	}
}

func TestEverySampleBandIsSimpleForEveryStyle(t *testing.T) {
	bundle := loadBundle(t)
	for _, glyph := range bundle.Glyphs {
		for _, contour := range glyph.Skeletons {
			for _, run := range contour.Runs {
				points := make([]geom.Point, len(run.Points))
				for i, p := range run.Points {
					points[i] = p.Position
				}
				for _, join := range []stroke.JoinStyle{stroke.JoinRound, stroke.JoinBevel, stroke.JoinMiter} {
					for _, tip := range []stroke.TipStyle{stroke.TipRound, stroke.TipFlat, stroke.TipSquare} {
						params := stroke.DefaultParams()
						params.Join = join
						params.Tip = tip
						band, err := stroke.Build(points, params, glyph.Glyph.Bounds.F(), 200000)
						if err != nil {
							t.Fatalf("%s contour %d curve %d join=%s tip=%s: %v",
								glyph.Glyph.ID, contour.ContourIndex, run.CurveIndex, join, tip, err)
						}
						if self, at := geom.PolygonSelfIntersects(band.Polygon); self {
							t.Fatalf("%s contour %d curve %d join=%s tip=%s self-intersects at %+v",
								glyph.Glyph.ID, contour.ContourIndex, run.CurveIndex, join, tip, at)
						}
					}
				}
			}
		}
	}
}
