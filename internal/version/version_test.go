package version

import (
	"testing"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/model"
)

func TestSnapshotsAreImmutableAndVersioned(t *testing.T) {
	initial := model.Params{
		ColorTolerance: 20, GradientThreshold: 8, StructureElementSize: 5,
		Seeds: []model.Point{{X: 5, Y: 6}},
	}
	store, first, err := New(32, 24, model.AlgorithmVersion, initial)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 {
		t.Fatalf("first version = %d", first.Version)
	}
	changed := initial
	changed.ColorTolerance = 25
	second, err := store.Update(changed)
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != 2 {
		t.Fatalf("second version = %d", second.Version)
	}
	if got := store.Latest(); got.Version != 2 || got.Params.ColorTolerance != 25 {
		t.Fatalf("latest = %+v", got)
	}
	old, err := store.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	if old.Params.ColorTolerance != 20 {
		t.Fatalf("old snapshot changed: %+v", old)
	}
	old.Params.Seeds[0].X = 999
	if store.Latest().Params.Seeds[0].X == 999 {
		t.Fatal("snapshot returned by Get was not a deep copy")
	}
	_, err = store.Get(99)
	if apperr.Code(err) != apperr.CodeVersion {
		t.Fatalf("missing version error = %v", err)
	}
}

func TestParameterBoundsAndNormalization(t *testing.T) {
	base := model.Params{
		ColorTolerance: 20, GradientThreshold: 8, StructureElementSize: 5,
		Seeds: []model.Point{{X: 7, Y: 3}, {X: 2, Y: 1}},
	}
	store, snap, err := New(16, 16, model.AlgorithmVersion, base)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Params.Seeds[0] != (model.Point{X: 2, Y: 1}) {
		t.Fatalf("seeds not normalized: %+v", snap.Params.Seeds)
	}
	bad := base
	bad.StructureElementSize = 4
	if _, err := store.Update(bad); apperr.Code(err) != apperr.CodePara || apperr.Field(err) != "structure_element_size" {
		t.Fatalf("bad parameter error = %v", err)
	}
	bad = base
	bad.GradientThreshold = -1
	if _, err := store.Update(bad); apperr.Code(err) != apperr.CodePara || apperr.Field(err) != "gradient_threshold" {
		t.Fatalf("bad parameter error = %v", err)
	}
}
