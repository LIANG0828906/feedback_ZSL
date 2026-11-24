package state

import (
	"testing"

	"hdrproof/internal/apperr"
)

func TestIllegalTransitionKeepsState(t *testing.T) {
	m := New([]string{"r1"}, map[string][]int{"r1": {0, 1}})
	if err := m.TransitionRoute("r1", RouteCompleted); err == nil || apperr.CodeOf(err) != apperr.CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if got := m.RoutePhase("r1"); got != RoutePending {
		t.Fatalf("route state changed to %s", got)
	}
	if err := m.TransitionRoute("r1", RouteRunning); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionSegment("r1", 0, SegmentCommitted); err == nil {
		t.Fatal("pending segment cannot commit directly")
	}
	if got := m.SegmentPhase("r1", 0); got != SegmentPending {
		t.Fatalf("segment state changed to %s", got)
	}
	if err := m.TransitionSegment("r1", 0, SegmentRunning); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionSegment("r1", 0, SegmentCommitted); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionSegment("r1", 0, SegmentRunning); err == nil || apperr.CodeOf(err) != apperr.CodeState {
		t.Fatalf("committed segment must be frozen, got %v", err)
	}
	if got := m.SegmentPhase("r1", 0); got != SegmentCommitted {
		t.Fatalf("committed state changed to %s", got)
	}
}

func TestFailedSegmentCanRetry(t *testing.T) {
	m := New([]string{"r1"}, map[string][]int{"r1": {0}})
	_ = m.TransitionRoute("r1", RouteRunning)
	_ = m.TransitionSegment("r1", 0, SegmentRunning)
	if err := m.TransitionSegment("r1", 0, SegmentFailed); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionSegment("r1", 0, SegmentRunning); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionSegment("r1", 0, SegmentCommitted); err != nil {
		t.Fatal(err)
	}
	if got := m.Progress(); got.SegmentsCommitted != 1 || got.RoutesCommitted != 1 {
		t.Fatalf("unexpected progress: %+v", got)
	}
}
