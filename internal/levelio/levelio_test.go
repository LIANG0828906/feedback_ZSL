package levelio

import (
	"encoding/json"
	"strings"
	"testing"

	"roomreverb/internal/model"
	"roomreverb/internal/sample"
)

func TestMissingFieldReturnsESchema(t *testing.T) {
	level := sample.Generate(42)
	data, _ := json.Marshal(level)
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	rooms := root["rooms"].([]any)
	delete(rooms[0].(map[string]any), "id")
	root["rooms"] = rooms
	mutated, _ := json.Marshal(root)

	_, err := Load(mutated)
	if err == nil {
		t.Fatal("expected schema error")
	}
	merr, ok := err.(*model.Error)
	if !ok || merr.Code != model.CodeSchema {
		t.Fatalf("expected E_SCHEMA, got %#v", err)
	}
	if !strings.Contains(merr.Ref, "rooms[0]") {
		t.Fatalf("error ref should identify rooms[0], got %q", merr.Ref)
	}
}

func TestOverlapAndDoorOutsideReturnEGeomWithRefs(t *testing.T) {
	overlap := sample.Generate(42)
	overlap.Rooms[3].Rect.Min.Y = 300
	err := Validate(overlap)
	if err == nil {
		t.Fatal("expected overlap geometry error")
	}
	merr, ok := err.(*model.Error)
	if !ok || merr.Code != model.CodeGeom || !strings.Contains(merr.Ref, "R1") || !strings.Contains(merr.Ref, "R4") {
		t.Fatalf("expected E_GEOM R1/R4, got %#v", err)
	}

	badDoor := sample.Generate(42)
	badDoor.Doors[0].A.Y = 10
	err = Validate(badDoor)
	if err == nil {
		t.Fatal("expected door geometry error")
	}
	merr, ok = err.(*model.Error)
	if !ok || merr.Code != model.CodeGeom || merr.Ref != badDoor.Doors[0].WallID {
		t.Fatalf("expected E_GEOM ref %s, got %#v", badDoor.Doors[0].WallID, err)
	}
}
