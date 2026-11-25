package audiofp_test

import (
	"testing"

	"audiofp"
	"audiofp/samplegen"
)

func annotatedSample(t *testing.T) *audiofp.AnnotationResult {
	t.Helper()
	m := samplegen.GenerateDefault()
	result, err := audiofp.AnnotateFull(nilContext(), m, audiofp.DefaultParams(), audiofp.ResourceLimits{MaxWorkers: 2})
	if err != nil {
		t.Fatalf("annotation failed: %v", err)
	}
	return result
}

func TestQualityMatchesManifest(t *testing.T) {
	m := samplegen.GenerateDefault()
	result := annotatedSample(t)
	if len(result.Intervals) != len(m.Manifest.Events) {
		t.Fatalf("expected %d intervals, got %d", len(m.Manifest.Events), len(result.Intervals))
	}
	if result.Quality == nil || !result.Quality.Passed {
		t.Fatalf("quality should pass: %#v", result.QualityError)
	}
	if result.Quality.CategoryAccuracy != 1 {
		t.Fatalf("category accuracy=%v", result.Quality.CategoryAccuracy)
	}
	if len(result.Quality.BoundaryDeviations) == 0 {
		t.Fatalf("expected frame-boundary deviations to be listed separately")
	}
	for _, interval := range result.Intervals {
		if interval.Confidence < 0 || interval.Confidence > 1 {
			t.Fatalf("confidence out of range: %#v", interval)
		}
	}
}

func TestTighteningToleranceIsMonotonic(t *testing.T) {
	m := samplegen.GenerateDefault()
	result := annotatedSample(t)
	params := audiofp.DefaultParams()
	tolerances := []int{1024, 512, 256, 128, 0}
	previous := len(m.Manifest.Events) + 1
	for _, tolerance := range tolerances {
		report, err := audiofp.EvaluateQuality(m.Manifest, result.Intervals, params, tolerance)
		if err != nil && !audiofp.HasCode(err, audiofp.CodeQual) {
			t.Fatalf("tolerance %d unexpected error: %v", tolerance, err)
		}
		if report.PassedCount > previous {
			t.Fatalf("tolerance %d passed count %d increased from %d", tolerance, report.PassedCount, previous)
		}
		previous = report.PassedCount
		t.Logf("tolerance=%d passed=%d", tolerance, report.PassedCount)
	}
}

func TestQualityCategoryMismatchListsBothValues(t *testing.T) {
	m := samplegen.GenerateDefault()
	result := annotatedSample(t)
	result.Intervals[0].Category = "music"
	report, err := audiofp.EvaluateQuality(m.Manifest, result.Intervals, audiofp.DefaultParams(), 512)
	if !audiofp.HasCode(err, audiofp.CodeQual) {
		t.Fatalf("expected E_QUAL, got %v", err)
	}
	if report == nil || len(report.Mismatches) == 0 {
		t.Fatalf("expected mismatch details")
	}
	found := false
	for _, mismatch := range report.Mismatches {
		if mismatch.Kind == "category_mismatch" && mismatch.Expected == "voice" && mismatch.Actual == "music" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing category mismatch with both values: %#v", report.Mismatches)
	}
}

func TestSameCategoryIntervalsDoNotOverlapAndAreGridAligned(t *testing.T) {
	result := annotatedSample(t)
	params := audiofp.DefaultParams()
	quantum := params.QuantumStep()
	byCategory := map[string][]audiofp.Interval{}
	for _, interval := range result.Intervals {
		if interval.StartSample%quantum != 0 || interval.EndSample%quantum != 0 {
			t.Fatalf("interval not grid aligned: %#v", interval)
		}
		byCategory[interval.Category] = append(byCategory[interval.Category], interval)
	}
	for category, intervals := range byCategory {
		for i := 1; i < len(intervals); i++ {
			if intervals[i-1].EndSample > intervals[i].StartSample {
				t.Fatalf("category %s overlaps: %#v %#v", category, intervals[i-1], intervals[i])
			}
		}
	}
}
