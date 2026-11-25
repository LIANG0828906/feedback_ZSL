package pipeline

import (
	"context"
	"reflect"
	"testing"
)

func loadVersion(t *testing.T, version string) *Project {
	t.Helper()
	sample, err := GenerateDefault()
	if err != nil {
		t.Fatal(err)
	}
	data, err := sample.VersionDocument(version)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := LoadJSON(data)
	if err != nil {
		t.Fatalf("load %s: %v", version, err)
	}
	return doc.Project
}

func TestThreeSchemaVersionsNormalizeIdentically(t *testing.T) {
	v1 := loadVersion(t, "v1")
	v2 := loadVersion(t, "v2")
	v3 := loadVersion(t, "v3")
	if !reflect.DeepEqual(v1.Features, v2.Features) || !reflect.DeepEqual(v1.Features, v3.Features) {
		t.Fatalf("normalized feature definitions differ")
	}
	if len(v1.Events) != len(v2.Events) || len(v1.Events) != len(v3.Events) {
		t.Fatalf("event count differs: %d %d %d", len(v1.Events), len(v2.Events), len(v3.Events))
	}
	for i := range v1.Events {
		e1, e2, e3 := v1.Events[i], v2.Events[i], v3.Events[i]
		if e1.Seq != e2.Seq || e1.Seq != e3.Seq ||
			e1.FeatureID != e2.FeatureID || e1.FeatureID != e3.FeatureID ||
			e1.ObservedAtMs != e2.ObservedAtMs || e1.ObservedAtMs != e3.ObservedAtMs ||
			e1.Channel != e2.Channel || e1.Channel != e3.Channel ||
			e1.Value != e2.Value || e1.Value != e3.Value {
			t.Fatalf("normalized event %d differs", i)
		}
	}
	p1, err := WindowPlans(v1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := WindowPlans(v2)
	if err != nil {
		t.Fatal(err)
	}
	p3, err := WindowPlans(v3)
	if err != nil {
		t.Fatal(err)
	}
	if !sameWindowKeys(p1, p2) || !sameWindowKeys(p1, p3) {
		t.Fatalf("window plans differ across versions")
	}
	if v1.WatermarkEnabled {
		t.Fatalf("v1 must explicitly disable watermark processing")
	}
	if !v2.WatermarkEnabled || !v3.WatermarkEnabled {
		t.Fatalf("v2/v3 must enable watermark processing")
	}
	id1, err := v1.Identity()
	if err != nil {
		t.Fatal(err)
	}
	id2, err := v2.Identity()
	if err != nil {
		t.Fatal(err)
	}
	id3, err := v3.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(id1, id2) || !reflect.DeepEqual(id1, id3) {
		t.Fatalf("structural identity differs across versions")
	}
}

func TestWatermarkDiscardAndLateRecompute(t *testing.T) {
	p := loadVersion(t, "v2")
	result, err := Run(context.Background(), p, RunOptions{
		Workers: 2, MaxWorkers: 8,
		WindowBufferPerFeature: 16, MaxWindowBufferPerFeature: 32,
		RatioThreshold: 3, CommitEvery: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Discards) < 2 {
		t.Fatalf("expected expired late events in discard set, got %d", len(result.Discards))
	}
	if result.RecomputeTotal == 0 {
		t.Fatalf("expected accepted late events to force recomputation")
	}
	for _, d := range result.Discards {
		if d.LateByMs <= 0 || d.Window.FeatureID != d.FeatureID || d.ReasonCode == "" {
			t.Fatalf("discard lacks window/late context: %+v", d)
		}
	}
}

func TestAggregateMatchesIndependentBruteForce(t *testing.T) {
	p := loadVersion(t, "v2")
	result, err := Run(context.Background(), p, DefaultRunOptions())
	if err != nil {
		t.Fatal(err)
	}
	plans, err := WindowPlans(p)
	if err != nil {
		t.Fatal(err)
	}
	eventBySeq := map[int64]ObservationEvent{}
	for _, e := range p.Events {
		eventBySeq[e.Seq] = e
	}
	for _, f := range p.Features {
		for _, key := range plans[f.ID].Windows {
			var accepted []ObservationEvent
			var discardCount int
			for _, e := range p.Events {
				if e.FeatureID != f.ID || e.ObservedAtMs < key.StartMs || e.ObservedAtMs >= key.EndMs {
					continue
				}
				if p.WatermarkEnabled {
					wm := *e.WatermarkMs
					if wm >= key.EndMs+f.AllowedLatenessMs {
						discardCount++
						continue
					}
				}
				accepted = append(accepted, e)
			}
			got, ok := result.WindowByKey(key)
			if !ok {
				t.Fatalf("missing window %s", key)
			}
			if got.ObservationCount != len(accepted) {
				t.Fatalf("%s count got %d want %d", key, got.ObservationCount, len(accepted))
			}
			for _, agg := range f.Aggregations {
				want := bruteAggregate(accepted, agg)
				actual, ok := got.Aggregate(agg.Kind)
				if !ok {
					t.Fatalf("%s missing aggregate %s", key, agg.Kind)
				}
				if actual.Value != want || actual.Weight != agg.Weight {
					t.Fatalf("%s %s got %d/%d want %d/%d", key, agg.Kind,
						actual.Value, actual.Weight, want, agg.Weight)
				}
			}
			if discardCount > 0 {
				found := 0
				for _, d := range result.Discards {
					if d.Window == key {
						found++
					}
				}
				if found != discardCount {
					t.Fatalf("%s discard rows got %d want %d", key, found, discardCount)
				}
			}
		}
	}
}

func bruteAggregate(events []ObservationEvent, agg Aggregation) int64 {
	if len(events) == 0 {
		return 0
	}
	switch agg.Kind {
	case AggCount:
		return int64(len(events))
	case AggSum:
		var sum int64
		for _, e := range events {
			sum += e.Value
		}
		return sum
	case AggAvg:
		var weighted, weights int64
		for _, e := range events {
			weighted += e.Value * agg.Weight
			weights += agg.Weight
		}
		return weighted / weights
	case AggWeightedSum:
		var weighted int64
		for _, e := range events {
			weighted += e.Value * agg.Weight
		}
		return weighted / NormalizedWeightScale
	case AggMin:
		min := events[0].Value
		for _, e := range events[1:] {
			if e.Value < min {
				min = e.Value
			}
		}
		return min
	case AggMax:
		max := events[0].Value
		for _, e := range events[1:] {
			if e.Value > max {
				max = e.Value
			}
		}
		return max
	default:
		return 0
	}
}

func TestWorkersAndCommitBatchesDoNotChangeResults(t *testing.T) {
	p := loadVersion(t, "v3")
	base, err := Run(context.Background(), p, RunOptions{
		Workers: 1, CommitEvery: 1, RatioThreshold: 3,
		WindowBufferPerFeature: 16, MaxWindowBufferPerFeature: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := Run(context.Background(), p, RunOptions{
		Workers: 4, CommitEvery: 9, RatioThreshold: 3,
		WindowBufferPerFeature: 16, MaxWindowBufferPerFeature: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base.Windows, parallel.Windows) {
		t.Fatalf("worker/commit scheduling changed window results")
	}
	if !reflect.DeepEqual(base.Discards, parallel.Discards) {
		t.Fatalf("worker/commit scheduling changed discards")
	}
	if !reflect.DeepEqual(base.PathDecisions, parallel.PathDecisions) {
		t.Fatalf("worker/commit scheduling changed path decisions")
	}
}

func TestBothTraversalPathsProduceIdenticalWindows(t *testing.T) {
	p := loadVersion(t, "v2")
	lowThreshold, err := Run(context.Background(), p, RunOptions{
		Workers: 2, RatioThreshold: 100, CommitEvery: 4,
		WindowBufferPerFeature: 16, MaxWindowBufferPerFeature: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	highThreshold, err := Run(context.Background(), p, RunOptions{
		Workers: 2, RatioThreshold: 1, CommitEvery: 7,
		WindowBufferPerFeature: 16, MaxWindowBufferPerFeature: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withoutTraversal(lowThreshold.Windows), withoutTraversal(highThreshold.Windows)) {
		t.Fatalf("cumulative and rescan paths differ")
	}
	if !reflect.DeepEqual(lowThreshold.Discards, highThreshold.Discards) {
		t.Fatalf("cumulative and rescan paths changed discards")
	}
	modes := map[TraversalMode]bool{}
	for _, d := range highThreshold.PathDecisions {
		modes[d.Mode] = true
	}
	if !modes[TraversalCumulative] || !modes[TraversalRescan] {
		t.Fatalf("expected both traversal modes, got %+v", lowThreshold.PathDecisions)
	}
}

func withoutTraversal(windows []WindowResult) []WindowResult {
	out := append([]WindowResult(nil), windows...)
	for i := range out {
		out[i].Traversal = ""
		out[i].PathReasonCode = ""
	}
	return out
}

func sameWindowKeys(a, b map[string]FeatureWindowPlan) bool {
	for id, pa := range a {
		pb, ok := b[id]
		if !ok || !reflect.DeepEqual(pa.Windows, pb.Windows) {
			return false
		}
	}
	return len(a) == len(b)
}
