package features

import (
	"testing"

	"stitchworkbench/internal/errs"
)

func TestParameterErrors(t *testing.T) {
	gray := make([]float32, 100*100)
	_, err := Extract(gray, 100, 100, Options{MaxFeatures: 1, ScaleLevels: 4, ResponseThreshold: 0.01})
	if e := errs.As(err); e.Code != errs.EPara || e.Field != "feature_budget" {
		t.Fatalf("feature budget error = %#v", e)
	}
	_, err = Extract(gray, 100, 100, Options{MaxFeatures: 1000, ScaleLevels: 9, ResponseThreshold: 0.01})
	if e := errs.As(err); e.Code != errs.EPara || e.Field != "scale_levels" {
		t.Fatalf("scale levels error = %#v", e)
	}
}
