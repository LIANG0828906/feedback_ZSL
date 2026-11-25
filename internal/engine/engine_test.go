package engine_test

import (
	"context"
	"reflect"
	"testing"

	"featurekernel/internal/engine"
	"featurekernel/internal/gen"
	"featurekernel/internal/kernel"
)

func TestGeneratedTruthPathsAndWorkerBudgets(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	truth, err := gen.BuildTruth(project)
	if err != nil {
		t.Fatal(err)
	}

	for _, workers := range []int{1, 2, 4, 8} {
		project.Budget.Workers = workers
		eng, err := engine.New(project)
		if err != nil {
			t.Fatal(err)
		}
		got, err := eng.Run(context.Background())
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if err := kernel.CompareWindowResults(truth.Expected.Windows, got.Windows, project.FixedTolerance); err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if !reflect.DeepEqual(got.RecomputedWindows, truth.Expected.RecomputedWindows) {
			t.Fatalf("workers=%d recomputed mismatch\nwant=%v\ngot =%v", workers, truth.Expected.RecomputedWindows, got.RecomputedWindows)
		}
		if !reflect.DeepEqual(got.Discarded, truth.Expected.Discarded) {
			t.Fatalf("workers=%d discarded mismatch\nwant=%+v\ngot =%+v", workers, truth.Expected.Discarded, got.Discarded)
		}
	}

	eng, err := engine.New(project)
	if err != nil {
		t.Fatal(err)
	}
	incremental, err := eng.RunWithPath(context.Background(), kernel.PathIncremental)
	if err != nil {
		t.Fatal(err)
	}
	rescan, err := eng.RunWithPath(context.Background(), kernel.PathRescan)
	if err != nil {
		t.Fatal(err)
	}
	if err := kernel.CompareWindowResults(incremental.Windows, rescan.Windows, project.FixedTolerance); err != nil {
		t.Fatalf("incremental vs rescan: %v", err)
	}
	if !reflect.DeepEqual(incremental.RecomputedWindows, rescan.RecomputedWindows) {
		t.Fatalf("path recomputed sets differ: %v vs %v", incremental.RecomputedWindows, rescan.RecomputedWindows)
	}
	if !reflect.DeepEqual(incremental.Discarded, rescan.Discarded) {
		t.Fatalf("path discarded sets differ")
	}
}

func TestWindowAttributionRulesAndHalfOpenBoundary(t *testing.T) {
	integerWindows, err := kernel.WindowsForTime(0, 180_000, 60_000, 120_000)
	if err != nil {
		t.Fatal(err)
	}
	if got := windowIndexes(integerWindows); !reflect.DeepEqual(got, []int64{0, 1, 2}) {
		t.Fatalf("integer ratio indexes = %v", got)
	}
	if rule := kernel.AttributionRule(180_000, 60_000); rule != kernel.AttributionInteger {
		t.Fatalf("rule = %s", rule)
	}

	boundaryWindows, err := kernel.WindowsForTime(0, 180_000, 60_000, 180_000)
	if err != nil {
		t.Fatal(err)
	}
	if got := windowIndexes(boundaryWindows); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatalf("half-open boundary indexes = %v", got)
	}
	originWindows, err := kernel.WindowsForTime(0, 180_000, 60_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := windowIndexes(originWindows); !reflect.DeepEqual(got, []int64{-2, -1, 0}) {
		t.Fatalf("origin overlap indexes = %v", got)
	}

	fractionalWindows, err := kernel.WindowsForTime(0, 150_000, 60_000, 120_000)
	if err != nil {
		t.Fatal(err)
	}
	if got := windowIndexes(fractionalWindows); !reflect.DeepEqual(got, []int64{0, 1, 2}) {
		t.Fatalf("fractional ratio indexes = %v", got)
	}
	if rule := kernel.AttributionRule(150_000, 60_000); rule != kernel.AttributionFractional {
		t.Fatalf("rule = %s", rule)
	}
}

func TestPathThresholdEqualityFallsBackToRescan(t *testing.T) {
	policy := kernel.PathPolicy{IncrementalRatioThreshold: 3, Boundary: kernel.PathBoundaryRule}
	equal := kernel.SelectPath(180_000, 60_000, policy)
	if equal.Path != kernel.PathRescan || equal.Boundary != kernel.PathBoundaryRule {
		t.Fatalf("equal threshold decision = %+v", equal)
	}
	above := kernel.SelectPath(300_000, 60_000, policy)
	if above.Path != kernel.PathIncremental {
		t.Fatalf("above threshold decision = %+v", above)
	}
}

func TestBufferBackpressureIsStableAndNonMutating(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	project.Budget.MaxWindowsPerFeature = 1
	_, err = engine.ProcessFeature(project, project.Features[0], "")
	if kernel.CodeOf(err) != kernel.CodeBackpressureBuffer {
		t.Fatalf("expected %s, got %v", kernel.CodeBackpressureBuffer, err)
	}
}

func TestWorkerBudgetExhaustionCode(t *testing.T) {
	pool, err := engine.NewWorkerPool(1)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	if err := pool.TrySubmit(func() {
		close(entered)
		<-release
	}); err != nil {
		t.Fatal(err)
	}
	<-entered
	err = pool.TrySubmit(func() {})
	if kernel.CodeOf(err) != kernel.CodeBackpressureThread {
		t.Fatalf("expected %s, got %v", kernel.CodeBackpressureThread, err)
	}
	close(release)
}

func TestBudgetBoundaries(t *testing.T) {
	if code := kernel.CodeOf(kernel.ValidateBudget(kernel.Budget{Workers: 0, MaxWindowsPerFeature: 10})); code != kernel.CodeBudgetWorkersOutOfRange {
		t.Fatalf("worker code = %s", code)
	}
	if code := kernel.CodeOf(kernel.ValidateBudget(kernel.Budget{Workers: 100, MaxWindowsPerFeature: 10})); code != kernel.CodeBudgetWorkersOutOfRange {
		t.Fatalf("worker upper code = %s", code)
	}
	if code := kernel.CodeOf(kernel.ValidateBudget(kernel.Budget{Workers: 2, MaxWindowsPerFeature: 0})); code != kernel.CodeBudgetBufferOutOfRange {
		t.Fatalf("buffer code = %s", code)
	}
	if code := kernel.CodeOf(kernel.ValidateBudget(kernel.Budget{Workers: 2, MaxWindowsPerFeature: kernel.MaxWindowBuffer + 1})); code != kernel.CodeBudgetBufferOutOfRange {
		t.Fatalf("buffer upper code = %s", code)
	}
}

func TestWatermarkBeforeSubmittedWindowCode(t *testing.T) {
	project, err := gen.BuildProject(gen.DefaultSeed, 0)
	if err != nil {
		t.Fatal(err)
	}
	feature := project.Features[0]
	runtime, err := engine.NewRuntime(project, feature, "")
	if err != nil {
		t.Fatal(err)
	}
	previous := project.TimeBaseMs + 13*60_000
	runtime.SetWatermark(&previous)
	runtime.HydrateState(engine.WindowState{
		Coord:     kernel.WindowAt(feature.AnchorMs, feature.WidthMs, feature.SlideMs, 0),
		Submitted: true,
	})
	err = runtime.ApplyRecord(kernel.Record{
		Seq: 999, Kind: kernel.RecordWatermark,
		Watermark: &kernel.Watermark{ArrivalSeq: 999, TimeMs: project.TimeBaseMs + 60_000},
	})
	if code := kernel.CodeOf(err); code != kernel.CodeWatermarkBeforeCommitted {
		t.Fatalf("expected %s, got %v", kernel.CodeWatermarkBeforeCommitted, err)
	}
}

func windowIndexes(windows []kernel.WindowCoord) []int64 {
	out := make([]int64, 0, len(windows))
	for _, window := range windows {
		out = append(out, window.Index)
	}
	return out
}
