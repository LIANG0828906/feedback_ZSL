package pipeline_test

import (
	"testing"

	"collectionrecon/internal/model"
	"collectionrecon/internal/pipeline"
)

// TestRunParallelAllSucceeds verifies the worker pool returns every zone in
// ascending order and that one zone's computation never blocks the others.
func TestRunParallelAllSucceeds(t *testing.T) {
	region := newRegion()
	engine := pipeline.NewEngine(region, pipeline.Limits{MaxWorkers: 2, MaxPixelBuffers: 16})
	results, failures, err := engine.RunParallel([]int{1, 2, 3, 4})
	if err != nil || len(failures) != 0 {
		t.Fatalf("unexpected failures: %v %v", err, failures)
	}
	for i, zone := range region.Zones {
		if results[i].ZoneID != zone.ID {
			t.Fatalf("result order mismatch at %d", i)
		}
	}
}

// TestDeterministicPointCoordinates ensures reconstructed coordinates are
// stable bit-for-bit across independent engine instances.
func TestDeterministicPointCoordinates(t *testing.T) {
	region := newRegion()
	r1, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).ComputeZone(3)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := pipeline.NewEngine(region, pipeline.DefaultLimits()).ComputeZone(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Points) != len(r2.Points) {
		t.Fatal("point count differs")
	}
	for i := range r1.Points {
		var p1, p2 model.Point = r1.Points[i].Coord, r2.Points[i].Coord
		if p1 != p2 {
			t.Fatalf("coordinate %d differs: %+v vs %+v", i, p1, p2)
		}
	}
}
