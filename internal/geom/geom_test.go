package geom_test

import (
	"testing"

	"collectionrecon/internal/geom"
	"collectionrecon/internal/model"
)

func TestClosedSimplePolygon(t *testing.T) {
	poly := model.Polygon{Vertices: []model.Point{
		{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0},
	}}
	if !geom.IsClosed(poly) || !geom.IsSimple(poly) {
		t.Fatal("rectangle should be closed and simple")
	}
	if !geom.PointInPolygon(model.Point{X: 5, Y: 5}, poly) {
		t.Fatal("interior point should be inside")
	}
	if geom.PointInPolygon(model.Point{X: 15, Y: 5}, poly) {
		t.Fatal("exterior point should be outside")
	}
}

func TestSelfIntersectDetected(t *testing.T) {
	bowtie := model.Polygon{Vertices: []model.Point{
		{0, 0}, {10, 10}, {10, 0}, {0, 10}, {0, 0},
	}}
	if geom.IsSimple(bowtie) {
		t.Fatal("bow-tie polygon must be flagged as self-intersecting")
	}
}

func TestRasterCompatibility(t *testing.T) {
	bbox := model.Rect{MinX: 0, MinY: 0, MaxX: 100, MaxY: 80}
	if err := geom.RasterCompatible(bbox, 4, 25, 20); err != nil {
		t.Fatalf("compatible raster rejected: %v", err)
	}
	if err := geom.RasterCompatible(bbox, 4, 24, 20); err == nil {
		t.Fatal("incompatible raster should be rejected")
	}
}

func TestRectOverlapAndContain(t *testing.T) {
	a := model.Rect{MinX: 0, MinY: 0, MaxX: 10, MaxY: 10}
	b := model.Rect{MinX: 10, MinY: 0, MaxX: 20, MaxY: 10}
	if geom.RectsOverlap(a, b) {
		t.Fatal("edge-touching rectangles must not count as overlapping")
	}
	inner := model.Rect{MinX: 1, MinY: 1, MaxX: 9, MaxY: 9}
	if !geom.RectInRect(inner, a) {
		t.Fatal("contained rect should be recognized")
	}
}
