package imaging

import (
	"errors"
	"testing"

	"iconbench/internal/model"
)

func TestDegenerateRegionError(t *testing.T) {
	err := ValidateDegenerate([]model.RegionStat{{ID: "R7", Area: 0, BBox: model.BBox{Width: 0, Height: 1}}})
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeDegen || me.FirstRegionID != "R7" {
		t.Fatalf("expected E_DEGEN R7, got %#v", err)
	}
}

func TestRegionCountMismatchListsExtra(t *testing.T) {
	mask := []byte{1, 0}
	labels, stats := ConnectedComponents(mask, 2, 1)
	if len(stats) != 1 {
		t.Fatalf("expected one component, got %d", len(stats))
	}
	rep := MatchRegions("img", stats, nil, labels, make([]int32, 2), 2, 1, model.DefaultTolerances())
	if rep.StateError == nil || rep.StateError.Code != model.CodeRegion {
		t.Fatalf("expected E_REGION, got %+v", rep.StateError)
	}
	if len(rep.Extra) != 1 || rep.Extra[0].ID != "R1" || rep.Extra[0].Area != 1 {
		t.Fatalf("unexpected extra refs: %+v", rep.Extra)
	}
}
