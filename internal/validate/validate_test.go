package validate

import (
	"encoding/json"
	"math"
	"testing"

	"roomreverb/internal/errs"
	"roomreverb/internal/generator"
	"roomreverb/internal/model"
)

func sample(t *testing.T) model.Level {
	t.Helper()
	b, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	return b.Level
}

func TestDecodeMissingRoomField(t *testing.T) {
	level := sample(t)
	raw, _ := json.Marshal(level)
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	rooms := obj["rooms"].([]any)
	params := rooms[0].(map[string]any)["params"].(map[string]any)
	delete(params, "reverb_gain")
	raw, _ = json.Marshal(obj)
	_, err := DecodeLevel(raw)
	if !errs.IsCode(err, errs.EDecode) {
		t.Fatalf("want E_DECODE got %v", err)
	}
	typed := err.(*errs.Error)
	if typed.Room != "r0" || typed.Field == "" {
		t.Fatalf("missing room/field context: %+v", typed)
	}
}

func TestDecodeNullRequiredField(t *testing.T) {
	level := sample(t)
	raw, _ := json.Marshal(level)
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	rooms := obj["rooms"].([]any)
	rooms[0].(map[string]any)["params"] = nil
	raw, _ = json.Marshal(obj)
	_, err := DecodeLevel(raw)
	if !errs.IsCode(err, errs.EDecode) {
		t.Fatalf("want E_DECODE got %v", err)
	}
	if err.(*errs.Error).Room != "r0" {
		t.Fatalf("null field lost room context: %+v", err)
	}
}

func TestOverlapReturnsERoomWithRoomID(t *testing.T) {
	level := sample(t)
	level.Rooms[1].Rect.X = level.Rooms[0].Rect.Right() - 50
	err := Validate(level)
	if !errs.IsCode(err, errs.ERoom) {
		t.Fatalf("want E_ROOM got %v", err)
	}
	if err.(*errs.Error).Room == "" {
		t.Fatalf("E_ROOM must identify a room: %+v", err)
	}
}

func TestWallEndpointMustBeFinite(t *testing.T) {
	level := sample(t)
	level.Walls[0].Start.X = math.Inf(1)
	err := Validate(level)
	if !errs.IsCode(err, errs.EDecode) {
		t.Fatalf("want E_DECODE got %v", err)
	}
}

func TestOpeningMustBeInsideWall(t *testing.T) {
	level := sample(t)
	level.Openings[0].Center.X += 2000
	err := Validate(level)
	if !errs.IsCode(err, errs.ERoom) {
		t.Fatalf("want E_ROOM got %v", err)
	}
	typed := err.(*errs.Error)
	if typed.Field == "" || typed.Room == "" {
		t.Fatalf("opening error lacks context: %+v", typed)
	}
}

func TestOpeningWidthMustBePositive(t *testing.T) {
	level := sample(t)
	level.Openings[0].Width = 0
	if err := Validate(level); !errs.IsCode(err, errs.ERoom) {
		t.Fatalf("want E_ROOM got %v", err)
	}
}

func TestListenerMustBeInsideRoom(t *testing.T) {
	level := sample(t)
	level.Listeners[0].Pos = model.Point{X: 10, Y: 10}
	err := Validate(level)
	if !errs.IsCode(err, errs.ERoom) {
		t.Fatalf("want E_ROOM got %v", err)
	}
	if err.(*errs.Error).Listener != "l0" {
		t.Fatalf("listener id missing: %+v", err)
	}
}

func TestParameterRangeReturnsEPara(t *testing.T) {
	level := sample(t)
	level.Rooms[2].Params.T60MS = 5
	err := Validate(level)
	if !errs.IsCode(err, errs.EPara) {
		t.Fatalf("want E_PARA got %v", err)
	}
	typed := err.(*errs.Error)
	if typed.Room != "r2" || typed.Field != "t60_ms" {
		t.Fatalf("parameter context: %+v", typed)
	}
}
