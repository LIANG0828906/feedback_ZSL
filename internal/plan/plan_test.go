package plan

import (
	"testing"

	"texlab/internal/apperr"
	"texlab/internal/model"
)

func validPlan() model.Plan {
	return model.Plan{
		Schema: "texlab-plan-v1", Name: "test", Revision: 1,
		Segments: model.SegmentSpec{Cols: 2, Rows: 2},
		Regions:  []model.Rect{{ID: "full", X: 0, Y: 0, W: 64, H: 48}},
		Entries: []model.Entry{{
			Index: 1, ID: "e1", TargetDepth: 16, TargetChannels: 4,
			Permutation: []int{2, 1, 0, 3}, Alpha: "straight", Quantize: "round",
			Regions: []string{"full"},
		}},
	}
}

func TestValidatePlanFields(t *testing.T) {
	im := model.NewImage(64, 48, 8, 4, "RGBA", "straight")
	if err := Validate(validPlan(), im); err != nil {
		t.Fatal(err)
	}
	p := validPlan()
	p.Entries[0].TargetDepth = 12
	err := Validate(p, im)
	if !apperr.IsCode(err, apperr.EPlan) || apperr.As(err).Field != "target_depth" || apperr.As(err).EntryIndex != 1 {
		t.Fatalf("unexpected error: %v", err)
	}
	p = validPlan()
	p.Entries[0].Permutation = []int{0, 0, 2, 3}
	err = Validate(p, im)
	if !apperr.IsCode(err, apperr.EPlan) || apperr.As(err).Field != "permutation[1]" {
		t.Fatalf("unexpected error: %v", err)
	}
	p = validPlan()
	p.Regions[0].W = 65
	if err := Validate(p, im); !apperr.IsCode(err, apperr.EPlan) {
		t.Fatalf("want E_PLAN, got %v", err)
	}
}

func TestValidateFillAndDrop(t *testing.T) {
	im := model.NewImage(64, 48, 8, 4, "RGBA", "straight")
	p := validPlan()
	p.Entries = []model.Entry{
		{Index: 1, ID: "drop", TargetDepth: 8, TargetChannels: 3, Permutation: []int{2, 1, 0}, Drop: []int{3}, Alpha: "straight", Quantize: "round", Regions: []string{"full"}},
		{Index: 2, ID: "add", TargetDepth: 8, TargetChannels: 4, Permutation: []int{0, 1, 2, -1}, Fill: []model.FillSpec{{TargetIndex: 3, Source: "constant", Channel: "A", Value: 255}}, Alpha: "straight", Quantize: "round", Regions: []string{"full"}},
	}
	if err := Validate(p, im); err != nil {
		t.Fatal(err)
	}
	p.Entries[0].Drop = []int{}
	if err := Validate(p, im); !apperr.IsCode(err, apperr.EPlan) || apperr.As(err).Field != "drop" {
		t.Fatalf("unexpected error: %v", err)
	}
}
