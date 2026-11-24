package core_test

import (
	"errors"
	"testing"

	"ocrworkbench/internal/core"
)

func TestBoxIoUAndDegenerateValidation(t *testing.T) {
	a := core.AxisBox(core.Rect{X: 10, Y: 20, Width: 100, Height: 40})
	b := core.AxisBox(core.Rect{X: 60, Y: 20, Width: 100, Height: 40})
	if iou := core.BoxIoU(a, a); iou != 1 {
		t.Fatalf("self IoU = %v", iou)
	}
	if iou := core.BoxIoU(a, b); iou <= 0 || iou >= 1 {
		t.Fatalf("overlap IoU = %v", iou)
	}
	disjoint := core.AxisBox(core.Rect{X: 500, Y: 500, Width: 10, Height: 10})
	if iou := core.BoxIoU(a, disjoint); iou != 0 {
		t.Fatalf("disjoint IoU = %v", iou)
	}
	if !core.ValidBox(a) {
		t.Fatal("valid rectangle rejected")
	}
	degenerate := core.AxisBox(core.Rect{X: 1, Y: 1, Width: 0, Height: 20})
	if core.ValidBox(degenerate) {
		t.Fatal("degenerate box accepted")
	}
}

func TestParameterErrorsContainFieldAndRange(t *testing.T) {
	segment := core.DefaultSegmentParams()
	segment.MinCharGap = -1
	err := core.ValidateSegmentParams(segment)
	var typed *core.Error
	if !errors.As(err, &typed) {
		t.Fatalf("expected typed error, got %v", err)
	}
	if typed.Code != core.CodePara || typed.Field != "min_char_gap" || typed.Range != "[0,64]" {
		t.Fatalf("unexpected parameter error: %+v", typed)
	}
}

func TestBandThresholdTracksMergeIoU(t *testing.T) {
	params := core.DefaultDetectorParams()
	required := core.RequiredBand(params)
	params.Band = required - 1
	err := core.ValidateDetectorParams(params)
	var typed *core.Error
	if !errors.As(err, &typed) {
		t.Fatalf("expected E_BAND, got %v", err)
	}
	if typed.Code != core.CodeBand {
		t.Fatalf("code = %s", typed.Code)
	}
	if typed.Boundary == "" {
		t.Fatal("E_BAND must identify a tile boundary")
	}
	lower := core.DefaultDetectorParams()
	lower.MergeIoU = 0.1
	higher := core.DefaultDetectorParams()
	higher.MergeIoU = 0.5
	if core.RequiredBand(lower) >= core.RequiredBand(higher) {
		t.Fatalf("required band must increase with merge IoU: %d >= %d", core.RequiredBand(lower), core.RequiredBand(higher))
	}
}
