package diagnostic_test

import (
	"testing"

	"terrain-occlusion/internal/diagnostic"
	"terrain-occlusion/internal/model"
)

func TestCompareLocatesFirstMismatchingSampleAndChunk(t *testing.T) {
	base := []model.SampleResult{
		{SampleIndex: 0, VisibleIDs: []int{1}, UncertainIDs: []int{2}},
		{SampleIndex: 1, VisibleIDs: []int{1}, UncertainIDs: []int{2}},
		{SampleIndex: 2, VisibleIDs: []int{1}, UncertainIDs: []int{2}},
	}
	resume := []model.SampleResult{
		base[0], base[1],
		{SampleIndex: 2, VisibleIDs: []int{1}, UncertainIDs: []int{3}},
	}
	replay := []model.SampleResult{base[0], base[1], base[2]}
	cmp := diagnostic.Compare(base, resume, replay)
	if cmp.Identical || cmp.FirstSample != 2 || cmp.FirstChunkID != 2 || cmp.Field != "uncertain_ids" {
		t.Fatalf("unexpected comparison: %+v", cmp)
	}
}

func TestCompareDetectsLengthDrift(t *testing.T) {
	cmp := diagnostic.Compare([]model.SampleResult{{}}, []model.SampleResult{}, []model.SampleResult{{}})
	if cmp.Identical || cmp.Field != "sample_count" || cmp.FirstSample != 0 {
		t.Fatalf("expected sample_count mismatch, got %+v", cmp)
	}
}
