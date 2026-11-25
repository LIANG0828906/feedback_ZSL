package pipeline

import (
	"context"
	"testing"
)

func TestIncrementalEventEditReusesUnaffectedWindows(t *testing.T) {
	oldProject := loadVersion(t, "v2")
	oldResult, err := Run(context.Background(), oldProject, DefaultRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	next := oldProject.Clone()
	var editedSeq int64
	for i := range next.Events {
		if next.Events[i].FeatureID == "F_TRAFFIC" && next.Events[i].ObservedAtMs == next.TimeOriginMs {
			editedSeq = next.Events[i].Seq
			next.Events[i].Value += 17_000
			break
		}
	}
	if editedSeq == 0 {
		t.Fatal("did not find event to edit")
	}
	increment, err := ApplyIncrement(context.Background(), oldProject, oldResult, next, DefaultRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !increment.MatchesFull {
		t.Fatalf("increment differs from full run: %v", increment.Mismatches)
	}
	if len(increment.RecomputedWindows) == 0 {
		t.Fatal("edit did not invalidate any windows")
	}
	if len(increment.ReusedWindows) == 0 {
		t.Fatal("unaffected windows were not reused")
	}
	foundEventSource := false
	foundDerived := false
	for _, inv := range increment.Invalidations {
		for _, source := range inv.Sources {
			if source.EventSeq == editedSeq {
				foundEventSource = true
			}
			if source.Kind == "dependency_closure" && source.ParentWindow != nil {
				foundDerived = true
			}
		}
	}
	if !foundEventSource || !foundDerived {
		t.Fatalf("missing event or derived invalidation source")
	}
}

func TestIncrementalAggregationEditPropagatesDependencyClosure(t *testing.T) {
	oldProject := loadVersion(t, "v2")
	oldResult, err := Run(context.Background(), oldProject, DefaultRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	next := oldProject.Clone()
	for i := range next.Features {
		if next.Features[i].ID == "F_TRAFFIC" {
			next.Features[i].Aggregations = next.Features[i].Aggregations[:1]
		}
	}
	increment, err := ApplyIncrement(context.Background(), oldProject, oldResult, next, DefaultRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !increment.MatchesFull {
		t.Fatalf("aggregation increment differs from full: %v", increment.Mismatches)
	}
	recomputedFeatures := map[string]int{}
	for _, key := range increment.RecomputedWindows {
		recomputedFeatures[key.FeatureID]++
	}
	if recomputedFeatures["F_TRAFFIC"] == 0 || recomputedFeatures["F_BLEND"] == 0 {
		t.Fatalf("dependency closure did not reach derived feature: %+v", recomputedFeatures)
	}
	if recomputedFeatures["F_LATENCY"] != 0 || recomputedFeatures["F_SCORE"] != 0 {
		t.Fatalf("unrelated feature was recomputed: %+v", recomputedFeatures)
	}
	if len(increment.Edits) != 1 || increment.Edits[0].Source.FeatureID != "F_TRAFFIC" {
		t.Fatalf("unexpected edit closures: %+v", increment.Edits)
	}
	if len(increment.Edits[0].PropagationClosure) <= len(increment.Edits[0].InitialWindows) {
		t.Fatalf("dependency closure did not expand: %+v", increment.Edits[0])
	}
}

func TestIncrementalNoOpReusesAllWindows(t *testing.T) {
	project := loadVersion(t, "v3")
	result, err := Run(context.Background(), project, DefaultRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	increment, err := ApplyIncrement(context.Background(), project.Clone(), result, project.Clone(), DefaultRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !increment.MatchesFull || len(increment.RecomputedWindows) != 0 ||
		len(increment.ReusedWindows) != len(result.Windows) {
		t.Fatalf("no-op edit did not reuse cache: recomputed=%d reused=%d total=%d",
			len(increment.RecomputedWindows), len(increment.ReusedWindows), len(result.Windows))
	}
}
