package model_test

import (
	"encoding/json"
	"testing"

	"levelacoustic/internal/generator"
	"levelacoustic/internal/model"
)

func clone(t *testing.T, p *model.Project) *model.Project {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func sample(t *testing.T) *model.Project {
	t.Helper()
	s, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	return s.Project
}

func TestValidateSchemaAndGeometryErrors(t *testing.T) {
	t.Run("missing truth", func(t *testing.T) {
		p := clone(t, sample(t))
		p.Truth = nil
		err := model.ValidateProject(p, model.ValidateOptions{RequireTruth: true})
		assertCode(t, err, model.ErrSchema)
	})
	t.Run("missing room id", func(t *testing.T) {
		p := clone(t, sample(t))
		p.Rooms[0].ID = ""
		err := model.ValidateProject(p, model.ValidateOptions{RequireTruth: true})
		assertCode(t, err, model.ErrSchema)
	})
	t.Run("overlapping rooms", func(t *testing.T) {
		p := clone(t, sample(t))
		p.Rooms[1].Rect.X = 850
		err := model.ValidateProject(p, model.ValidateOptions{RequireTruth: true})
		me := assertCode(t, err, model.ErrGeometry)
		if me.RoomID == "" {
			t.Fatalf("geometry error must name a room: %+v", me)
		}
	})
	t.Run("door outside wall", func(t *testing.T) {
		p := clone(t, sample(t))
		p.Doors[0].Start = 10_000
		err := model.ValidateProject(p, model.ValidateOptions{RequireTruth: true})
		me := assertCode(t, err, model.ErrGeometry)
		if me.WallID == "" {
			t.Fatalf("geometry error must name a wall: %+v", me)
		}
	})
	t.Run("source outside bounds", func(t *testing.T) {
		p := clone(t, sample(t))
		p.Sources[0].X = -1
		err := model.ValidateProject(p, model.ValidateOptions{RequireTruth: true})
		assertCode(t, err, model.ErrGeometry)
	})
}

func TestValidateGeneratedProject(t *testing.T) {
	if err := model.ValidateProject(sample(t), model.ValidateOptions{RequireTruth: true}); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeProjectDetectsMissingJSONField(t *testing.T) {
	p := sample(t)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	delete(root, "doors")
	data, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.DecodeProject(data, true)
	me := assertCode(t, err, model.ErrSchema)
	if me.Field != "project.doors" {
		t.Fatalf("missing field path = %s", me.Field)
	}
}

func assertCode(t *testing.T, err error, code string) *model.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	me, ok := model.AsError(err)
	if !ok {
		t.Fatalf("expected model error, got %T: %v", err, err)
	}
	if me.Code != code {
		t.Fatalf("expected %s, got %s (%v)", code, me.Code, err)
	}
	return me
}
