package compare

import (
	"testing"

	"audiobench/internal/model"
)

func TestCompareDetectsFirstFrameDifference(t *testing.T) {
	a := Results{Fingerprints: []model.Fingerprint{
		{Frame: 0, Quantized: []int{1, 2}},
		{Frame: 1, Quantized: []int{3, 4}},
	}}
	b := Results{Fingerprints: []model.Fingerprint{
		{Frame: 0, Quantized: []int{1, 2}},
		{Frame: 1, Quantized: []int{3, 9}},
	}}
	err := Compare("left", "right", a, b)
	if err == nil || err.Code != model.EDiv {
		t.Fatalf("期望 E_DIV，实际 %v", err)
	}
	if err.Details["index"] != 1 || err.Details["field"] != "quantized" {
		t.Fatalf("未定位到首个不一致帧/字段: %v", err.Details)
	}
}

func TestCompareDetectsEventFieldDifference(t *testing.T) {
	a := Results{Events: []model.Event{{Label: "shout", StartSample: 10, Confidence: 0.9}}}
	b := Results{Events: []model.Event{{Label: "shout", StartSample: 20, Confidence: 0.9}}}
	err := Compare("x", "y", a, b)
	if err == nil || err.Details["field"] != "start_sample" {
		t.Fatalf("期望定位 start_sample，实际 %v", err)
	}
}

func TestCompareEqualReturnsNil(t *testing.T) {
	r := Results{
		Fingerprints: []model.Fingerprint{{Frame: 0, Quantized: []int{1}}},
		Events:       []model.Event{{Label: "alarm"}},
		Quality:      &model.QualityReport{Matched: 1, LabelAccuracy: 1},
	}
	if err := Compare("a", "b", r, r); err != nil {
		t.Fatalf("相同产物不应报 E_DIV: %v", err)
	}
}
