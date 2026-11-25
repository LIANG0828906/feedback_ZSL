package raster

import (
	"testing"

	"strokeforge/internal/model"
)

func TestFrameDimensionFormula(t *testing.T) {
	bbox := model.BBox{MinX: -97, MinY: -120.1, MaxX: 97.2, MaxY: 151}
	params := model.StrokeParams{Scale: 1.5, BBoxPadding: 16}
	f := NewFrame(bbox, params)
	// ceil((maxX-minX+2*pad)*scale)
	if f.Width != 340 || f.Height != 455 {
		t.Fatalf("frame = %dx%d", f.Width, f.Height)
	}
}

func TestStampCoverageAndMerge(t *testing.T) {
	bbox := model.BBox{MinX: -10, MinY: -10, MaxX: 10, MaxY: 10}
	params := model.StrokeParams{Scale: 2, BBoxPadding: 2}
	f := NewFrame(bbox, params)
	m := NewMask(f)
	square := model.Polygon{Points: []model.FPoint{
		{X: -2, Y: -2}, {X: 2, Y: -2}, {X: 2, Y: 2}, {X: -2, Y: 2},
	}}
	m.CompositeStamp(square, f)
	covered := 0
	for _, v := range m.Pix {
		if v > 0 {
			covered++
		}
	}
	if covered == 0 {
		t.Fatal("stamp produced no coverage")
	}
	// MergeMax must be order independent and never reduce coverage.
	m2 := NewMask(f)
	m2.CompositeStamp(square, f)
	before := covered
	if err := m.MergeMax(m2); err != nil {
		t.Fatal(err)
	}
	after := 0
	for _, v := range m.Pix {
		if v > 0 {
			after++
		}
	}
	if after < before {
		t.Fatal("merge reduced coverage")
	}
}

func TestMaskEqualLocatesFirstDifference(t *testing.T) {
	bbox := model.BBox{MinX: 0, MinY: 0, MaxX: 4, MaxY: 4}
	params := model.StrokeParams{Scale: 1, BBoxPadding: 0}
	f := NewFrame(bbox, params)
	a := NewMask(f)
	b := NewMask(f)
	b.Pix[2*b.Width+3] = 200
	eq, x, y := a.Equal(b)
	if eq || x != 3 || y != 2 {
		t.Fatalf("eq=%v at (%d,%d)", eq, x, y)
	}
}
