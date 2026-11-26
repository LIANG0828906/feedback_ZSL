package silhouette

import (
	"context"
	"reflect"
	"testing"
)

func TestSamplingDeterministicAndWorkerIndependent(t *testing.T) {
	scene := GenerateScene(DefaultSeed)
	base, err := SampleScene(scene, SamplingOptions{Tolerance: DefaultTolerance, Budget: 100000, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := SampleScene(scene, SamplingOptions{Tolerance: DefaultTolerance, Budget: 100000, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base.Segments, repeat.Segments) {
		t.Fatal("repeated sampling differs")
	}
	for _, workers := range []int{2, 4} {
		parallel, err := SampleSceneContext(context.Background(), scene, SamplingOptions{Tolerance: DefaultTolerance, Budget: 100000, Workers: workers, VerticesPerWorker: DefaultVerticesPerWorker})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(base.Segments, parallel.Segments) {
			t.Fatalf("workers=%d changed sampled vertices", workers)
		}
	}
}

func TestToleranceTighteningIsMonotonic(t *testing.T) {
	scene := GenerateScene(DefaultSeed)
	tolerances := []int64{200000, 100000, 50000, 25000}
	previousCount := map[string]int{}
	previousChord := map[string]int64{}
	for _, tolerance := range tolerances {
		result, err := SampleScene(scene, SamplingOptions{Tolerance: tolerance, Budget: 100000})
		if err != nil {
			t.Fatal(err)
		}
		for _, sample := range result.Segments {
			if sample.FlatnessBound > tolerance || sample.MaxChordHeight > tolerance {
				t.Fatalf("segment %s exceeds tolerance: flatness=%d chord=%d tolerance=%d", sample.SegmentID, sample.FlatnessBound, sample.MaxChordHeight, tolerance)
			}
			if count, ok := previousCount[sample.SegmentID]; ok && len(sample.Vertices) < count {
				t.Fatalf("segment %s vertex count decreased: %d -> %d", sample.SegmentID, count, len(sample.Vertices))
			}
			if chord, ok := previousChord[sample.SegmentID]; ok && sample.MaxChordHeight > chord {
				t.Fatalf("segment %s max chord height increased: %d -> %d", sample.SegmentID, chord, sample.MaxChordHeight)
			}
			previousCount[sample.SegmentID] = len(sample.Vertices)
			previousChord[sample.SegmentID] = sample.MaxChordHeight
		}
	}
}

func TestToleranceBelowFloorReturnsTOL(t *testing.T) {
	scene := GenerateScene(DefaultSeed)
	_, err := SampleSegment(scene.Segments[0], 0, MaxSubdivisionDepth)
	var de *DomainError
	if !asDomainError(err, &de) {
		t.Fatalf("want DomainError, got %v", err)
	}
	if de.Code != CodeTolerance || de.Details["segment_id"] == "" || de.Details["measured"] == "" {
		t.Fatalf("unexpected E_TOL details: %+v", de)
	}
}

func TestBudgetDegradationIsRecordedAndRepeatable(t *testing.T) {
	scene := GenerateScene(DefaultSeed)
	a, err := SampleScene(scene, SamplingOptions{Tolerance: DefaultTolerance, Budget: 90, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := SampleScene(scene, SamplingOptions{Tolerance: DefaultTolerance, Budget: 90, Workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if a.Budget.FinalVertices > 90 {
		t.Fatalf("budget exceeded: %d", a.Budget.FinalVertices)
	}
	if len(a.Budget.Degradations) == 0 {
		t.Fatal("expected degradation records")
	}
	if !reflect.DeepEqual(a.Budget, b.Budget) {
		t.Fatal("degradation depends on worker count")
	}
	if !reflect.DeepEqual(a.Segments, b.Segments) {
		t.Fatal("degraded samples depend on worker count")
	}
	for _, item := range a.Budget.Degradations {
		if item.Basis == "" || item.ToTolerance <= item.FromTolerance || item.BudgetAfter < item.BudgetBefore && item.AfterVertices > item.BeforeVertices {
			t.Fatalf("incomplete degradation record: %+v", item)
		}
	}
	_, err = SampleScene(scene, SamplingOptions{Budget: 3})
	var de *DomainError
	if !asDomainError(err, &de) {
		t.Fatalf("want E_BUDGET, got %v", err)
	}
	if de.Code != CodeBudget || de.Details["overage_vertices"] == "" || de.Details["last_priority_segment"] == "" {
		t.Fatalf("unexpected E_BUDGET details: %+v", de)
	}
}
