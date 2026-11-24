package vision_test

import (
	"testing"

	"maskpipe/internal/model"
	"maskpipe/internal/vision"
)

func TestParameterValidationFieldNames(t *testing.T) {
	base := model.ParameterSet{
		ColorTolerance: 64, GradientThreshold: 48, StructureSize: 3,
		BinaryThreshold: 0.12, MinRegionArea: 10, MaxIterations: 100,
	}
	cases := []struct {
		name   string
		mutate func(*model.ParameterSet)
		field  string
	}{
		{"color", func(p *model.ParameterSet) { p.ColorTolerance = 999 }, "color_tolerance"},
		{"gradient", func(p *model.ParameterSet) { p.GradientThreshold = 0 }, "gradient_threshold"},
		{"structure", func(p *model.ParameterSet) { p.StructureSize = 4 }, "structure_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			err := vision.ValidateParameters(p)
			pe, ok := model.AsError(err)
			if !ok || pe.Code != model.CodePara || pe.Field != tc.field {
				t.Fatalf("expected E_PARA field %s, got %v", tc.field, err)
			}
		})
	}
}

func TestRegionCountAndQualityErrors(t *testing.T) {
	truth := []model.Region{{ID: 1, Area: 100, Centroid: model.Point{X: 10, Y: 10}, BBox: model.BBox{MinX: 9, MinY: 9, MaxX: 11, MaxY: 11}}}
	pred := []model.Region{
		{ID: 1, Area: 100, Centroid: model.Point{X: 10, Y: 10}, BBox: truth[0].BBox},
		{ID: 2, Area: 40, Centroid: model.Point{X: 50, Y: 50}, BBox: model.BBox{}},
	}
	report := vision.EvaluateRegions(pred, truth, model.QualityTolerance{CentroidPixels: 1, AreaRelative: 0.05, BBoxPixels: 1})
	if report.OK {
		t.Fatal("expected region count mismatch")
	}
	if len(report.Extra) != 1 || report.Extra[0].Area != 40 {
		t.Fatalf("expected one extra area 40, got %+v", report.Extra)
	}

	bad := []model.Region{{ID: 1, Area: 200, Centroid: model.Point{X: 15, Y: 10}, BBox: model.BBox{MinX: 9, MinY: 9, MaxX: 14, MaxY: 11}}}
	quality := vision.EvaluateRegions(bad, truth, model.QualityTolerance{CentroidPixels: 1, AreaRelative: 0.05, BBoxPixels: 1})
	if quality.OK || len(quality.QualityErrors) == 0 || quality.QualityErrors[0].Code != model.CodeQual {
		t.Fatalf("expected E_QUAL, got %+v", quality)
	}
}
