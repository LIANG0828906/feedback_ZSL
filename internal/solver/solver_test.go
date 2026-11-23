package solver_test

import (
	"bytes"
	"context"
	"math"
	"testing"

	"roomreverb/internal/errs"
	"roomreverb/internal/fixed"
	"roomreverb/internal/generator"
	"roomreverb/internal/model"
	"roomreverb/internal/solver"
)

func testSample(t *testing.T) (model.Level, model.Truth) {
	t.Helper()
	b, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	return b.Level, b.Truth
}

func TestPathsAndAcousticsMatchTruth(t *testing.T) {
	level, truth := testSample(t)
	g := solver.BuildGraph(level)
	for _, tp := range truth.Pairs {
		got, err := solver.ShortestPath(g, tp.SourceID, tp.ListenerID, 64)
		if err != nil {
			t.Fatalf("%s/%s: %v", tp.SourceID, tp.ListenerID, err)
		}
		if tp.Reachable != true {
			t.Fatalf("unexpected unreachable truth")
		}
		if !equal(got.Rooms, tp.RoomPath) || !equal(got.Portals, tp.DoorPath) {
			t.Fatalf("%s/%s path want %v/%v got %v/%v", tp.SourceID, tp.ListenerID, tp.RoomPath, tp.DoorPath, got.Rooms, got.Portals)
		}
		est := solver.Estimate(level, got.Rooms, got.Length)
		if math.Abs(fixed.Value(got.Length).Float()-tp.PathMM) > truth.Tolerance.PathMM {
			t.Fatalf("path length mismatch")
		}
		if math.Abs(fixed.Value(est.Decay).Float()-tp.DecayMS) > truth.Tolerance.DecayMS {
			t.Fatalf("decay mismatch")
		}
		if math.Abs(fixed.Value(est.Energy).Float()-tp.EarlyEnergy) > truth.Tolerance.EarlyEnergy {
			t.Fatalf("energy mismatch")
		}
	}
}

func TestDisconnectedReturnsEConnWithBothIDs(t *testing.T) {
	level, _ := testSample(t)
	for i := range level.Openings {
		level.Openings[i].Active = false
	}
	g := solver.BuildGraph(level)
	_, err := solver.ShortestPath(g, "s0", "l5", 64)
	if !errs.IsCode(err, errs.EConn) {
		t.Fatalf("want E_CONN got %v", err)
	}
	typed := err.(*errs.Error)
	if typed.Source != "s0" || typed.Listener != "l5" {
		t.Fatalf("source/listener context: %+v", typed)
	}
}

func TestDecayMonotonicWhenT60Tightens(t *testing.T) {
	level, _ := testSample(t)
	g := solver.BuildGraph(level)
	path, err := solver.ShortestPath(g, "s2", "l0", 64)
	if err != nil {
		t.Fatal(err)
	}
	before := solver.Estimate(level, path.Rooms, path.Length).Decay
	for _, roomID := range path.Rooms {
		idx := -1
		for i := range level.Rooms {
			if level.Rooms[i].ID == roomID {
				idx = i
				break
			}
		}
		level.Rooms[idx].Params.T60MS *= 0.5
	}
	after := solver.Estimate(level, path.Rooms, path.Length).Decay
	if after > before {
		t.Fatalf("decay increased after tightening T60: %d > %d", after, before)
	}
}

func TestPathBufferLimitReturnsResourceError(t *testing.T) {
	level, _ := testSample(t)
	g := solver.BuildGraph(level)
	_, err := solver.ShortestPath(g, "s2", "l0", 1)
	if !errs.IsCode(err, errs.EResource) {
		t.Fatalf("want E_RESOURCE got %v", err)
	}
}

func TestParallelResultsDeterministicAndBounded(t *testing.T) {
	level, _ := testSample(t)
	ids := []string{"r0", "r1", "r2", "r3", "r4", "r5"}
	for i := 0; i < 30; i++ {
		ids = append(ids, "r0", "r1", "r2", "r3", "r4", "r5")
	}
	ctx := context.Background()
	one := solver.SolveRooms(ctx, level, ids, solver.Config{MaxParallel: 1, PathBufferLimit: 64, GraphCacheBudget: 1})
	many := solver.SolveRooms(ctx, level, ids, solver.Config{MaxParallel: 3, PathBufferLimit: 64, GraphCacheBudget: 1})
	if one.Failures != nil && len(one.Failures) > 0 {
		t.Fatalf("serial failures: %v", one.Failures)
	}
	if len(many.Failures) > 0 {
		t.Fatalf("parallel failures: %v", many.Failures)
	}
	if many.Metrics.PeakWorkers > 3 || many.Metrics.PeakWorkers < 1 {
		t.Fatalf("peak workers out of bounds: %d", many.Metrics.PeakWorkers)
	}
	if many.Metrics.QueueBlocked == 0 {
		t.Fatal("expected bounded queue to block under load")
	}
	if many.Metrics.CacheBudgetWaits == 0 {
		t.Fatal("expected graph cache budget wait to be recorded")
	}
	reasons := map[string]bool{}
	for _, event := range many.Metrics.Events {
		reasons[event.Kind] = event.Reason != ""
	}
	if !reasons["worker_queue"] || !reasons["graph_cache"] {
		t.Fatalf("resource events lack blocking reasons: %+v", many.Metrics.Events)
	}
	if many.Metrics.PathBufferHigh > 64 {
		t.Fatal("path buffer high-water exceeded limit")
	}
	for _, id := range ids {
		if !bytes.Equal(one.Rooms[id].CanonicalJSON(), many.Rooms[id].CanonicalJSON()) {
			t.Fatalf("parallel output differs for %s", id)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
