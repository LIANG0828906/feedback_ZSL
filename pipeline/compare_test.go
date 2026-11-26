package pipeline

import (
	"testing"

	"iconflow/errs"
	"iconflow/model"
	"iconflow/segment"
)

func TestCompareRunsLocatesThresholdDiff(t *testing.T) {
	coord := model.BlockCoord{ImageID: "img", TX: 1, TY: 2}
	expected := &RunReport{Results: []*BlockResult{{
		Coord: coord, Threshold: &segment.ThresholdResult{Threshold: 120},
	}}}
	actual := &RunReport{Results: []*BlockResult{{
		Coord: coord, Threshold: &segment.ThresholdResult{Threshold: 121},
	}}}
	err := CompareRuns(expected, actual)
	if err == nil || err.Code != errs.EDiff {
		t.Fatalf("expected E_DIFF, got %+v", err)
	}
	if err.Details["coord"] != coord.Key() || err.Details["field"] != "threshold" {
		t.Fatalf("diff did not locate coord/field: %+v", err.Details)
	}
}
