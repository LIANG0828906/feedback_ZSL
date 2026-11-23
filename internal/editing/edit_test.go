package editing_test

import (
	"testing"

	"roomreverb/internal/editing"
	"roomreverb/internal/generator"
	"roomreverb/internal/model"
)

func TestMoveSharedBoundaryUpdatesBothRoomsWallAndPortal(t *testing.T) {
	b, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := editing.Apply(b.Level, model.Edit{
		Kind: model.EditMoveRoomEdge, RoomID: "r3", Edge: "right", DeltaMM: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	r3, _ := applied.Level.Room("r3")
	r4, _ := applied.Level.Room("r4")
	oldR3, _ := b.Level.Room("r3")
	oldR4, _ := b.Level.Room("r4")
	if r3.Rect.W != oldR3.Rect.W+50 || r4.Rect.X != oldR4.Rect.X+50 || r4.Rect.W != oldR4.Rect.W-50 {
		t.Fatalf("shared rooms not adjusted: r3=%+v r4=%+v", r3.Rect, r4.Rect)
	}
	w, _ := applied.Level.Wall("w34")
	if w.Start.X != 2250 || w.End.X != 2250 {
		t.Fatalf("shared wall not moved: %+v", w)
	}
	o, _ := applied.Level.Opening("o34")
	if o.Center.X != 2250 {
		t.Fatalf("portal did not follow its wall: %+v", o)
	}
}

func TestAddAndRemovePortal(t *testing.T) {
	b, _ := generator.Generate(42)
	portal := model.Opening{ID: "oX", WallID: "w45", Center: model.Point{X: 4000, Y: 2500}, Width: 200, Active: true}
	added, err := editing.Apply(b.Level, model.Edit{Kind: model.EditAddPortal, Portal: &portal})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := added.Level.Opening("oX"); !ok {
		t.Fatal("portal not added")
	}
	removed, err := editing.Apply(added.Level, model.Edit{Kind: model.EditRemovePortal, PortalID: "oX"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := removed.Level.Opening("oX"); ok {
		t.Fatal("portal not removed")
	}
}
