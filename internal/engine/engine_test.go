package engine

import (
	"testing"

	"strokeforge/internal/model"
)

func TestStateTransitions(t *testing.T) {
	cases := []struct {
		from, to string
		wantErr  bool
	}{
		{StatePending, StateProcessing, false},
		{StateProcessing, StateCommitted, false},
		{StateProcessing, StateFailed, false},
		{StateFailed, StatePending, false},
		{StateCommitted, StateProcessing, true},
		{StatePending, StateCommitted, true},
		{StateCommitted, StateFailed, true},
	}
	for _, c := range cases {
		err := Transition(c.from, c.to)
		if c.wantErr {
			if err == nil {
				t.Fatalf("expected error for %s->%s", c.from, c.to)
			}
			if ce, ok := err.(*model.CodedError); !ok || ce.Code != model.EState {
				t.Fatalf("want E_STATE, got %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unexpected error %s->%s: %v", c.from, c.to, err)
		}
	}
}

func TestSelfIntersectionAndZeroLength(t *testing.T) {
	// Bow-tie open polyline with a real crossing.
	bowtie := []model.FPoint{
		{X: 0, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 10, Y: 0}, {X: 12, Y: 4},
	}
	hit, i, j := selfIntersectOpen(bowtie)
	if !hit {
		t.Fatal("expected self-intersection")
	}
	if i != 0 || j != 2 {
		t.Fatalf("unexpected edges %d,%d", i, j)
	}
	zero := []model.FPoint{{X: 0, Y: 0}, {X: 0, Y: 0}, {X: 4, Y: 4}}
	if idx := zeroLengthOpenEdge(zero); idx != 0 {
		t.Fatalf("expected zero edge at 0, got %d", idx)
	}
	good := []model.FPoint{{X: 0, Y: 0}, {X: 4, Y: 0}, {X: 4, Y: 4}}
	if zeroLengthOpenEdge(good) >= 0 {
		t.Fatal("unexpected zero length")
	}
}

func TestArcTableSliceCoversEndpoints(t *testing.T) {
	pts := []model.FPoint{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}}
	table := BuildArcTable(pts)
	if got := table.AtRatio(0); got != pts[0] {
		t.Fatalf("ratio 0 = %v", got)
	}
	half := table.AtRatio(0.5)
	if half.X != 10 || half.Y != 10 {
		t.Fatalf("ratio 0.5 = %v", half)
	}
	slice := table.Slice(0, 0.25)
	if len(slice) < 2 {
		t.Fatalf("slice too short: %d", len(slice))
	}
}

func TestFlattenContourRejectsBadChain(t *testing.T) {
	c := model.Contour{
		ID:     "c",
		Closed: true,
		Segments: []model.Segment{
			{ID: "a", Typ: model.SegQuadratic, P0: model.Point{X: 0, Y: 0}, C1: model.Point{X: 1, Y: 1}, P1: model.Point{X: 2, Y: 0}},
			{ID: "b", Typ: model.SegQuadratic, P0: model.Point{X: 9, Y: 9}, C1: model.Point{X: 1, Y: 1}, P1: model.Point{X: 0, Y: 0}},
		},
	}
	if _, err := FlattenContour(c, 0.5); err == nil {
		t.Fatal("expected continuity error")
	}
}

func TestCacheKeyStableAndInputSensitive(t *testing.T) {
	seg := model.StrokeSegment{ID: 1, ContourID: "c", StartRatio: 0.2, EndRatio: 0.4, Upstream: []int{0}}
	k1 := SegmentCacheKey("hashA", "paramA", seg, []string{"up"})
	k2 := SegmentCacheKey("hashA", "paramA", seg, []string{"up"})
	if k1 != k2 {
		t.Fatal("cache keys not deterministic")
	}
	k3 := SegmentCacheKey("hashB", "paramA", seg, []string{"up"})
	if k1 == k3 {
		t.Fatal("content hash change did not alter key")
	}
	k4 := SegmentCacheKey("hashA", "paramA", seg, []string{"different"})
	if k1 == k4 {
		t.Fatal("upstream change did not alter key")
	}
}
