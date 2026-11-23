package validate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"

	"roomreverb/internal/errs"
	"roomreverb/internal/model"
)

type Bundle struct {
	Level model.Level
	Truth model.Truth
}

func LoadFiles(levelPath, truthPath string) (Bundle, error) {
	levelBytes, err := os.ReadFile(levelPath)
	if err != nil {
		return Bundle{}, &errs.Error{Code: errs.EInput, Field: "level", Message: err.Error(), Wrapped: err}
	}
	truthBytes, err := os.ReadFile(truthPath)
	if err != nil {
		return Bundle{}, &errs.Error{Code: errs.EInput, Field: "truth", Message: err.Error(), Wrapped: err}
	}
	level, err := DecodeLevel(levelBytes)
	if err != nil {
		return Bundle{}, err
	}
	truth, err := DecodeTruth(truthBytes)
	if err != nil {
		return Bundle{}, err
	}
	if err := Validate(level); err != nil {
		return Bundle{}, err
	}
	return Bundle{Level: level, Truth: truth}, nil
}

func DecodeLevel(data []byte) (model.Level, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return model.Level{}, errs.Decode("", "", "invalid JSON: %v", err)
	}
	if err := requireKeys(raw, "", "", "schema_version", "id", "seed", "units", "rooms", "walls", "openings", "sources", "listeners"); err != nil {
		return model.Level{}, err
	}
	if err := checkRoomsRaw(raw["rooms"]); err != nil {
		return model.Level{}, err
	}
	if err := checkWallsRaw(raw["walls"]); err != nil {
		return model.Level{}, err
	}
	if err := checkOpeningsRaw(raw["openings"]); err != nil {
		return model.Level{}, err
	}
	if err := checkPointsRaw(raw["sources"], "source"); err != nil {
		return model.Level{}, err
	}
	if err := checkPointsRaw(raw["listeners"], "listener"); err != nil {
		return model.Level{}, err
	}
	var level model.Level
	if err := json.Unmarshal(data, &level); err != nil {
		return model.Level{}, errs.Decode("", "", "invalid level JSON: %v", err)
	}
	if level.SchemaVersion != model.SchemaVersion {
		return model.Level{}, errs.Decode("", "schema_version", "want %q got %q", model.SchemaVersion, level.SchemaVersion)
	}
	if level.Units != "mm" {
		return model.Level{}, errs.Decode("", "units", "unsupported units %q", level.Units)
	}
	return level, nil
}

func DecodeTruth(data []byte) (model.Truth, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return model.Truth{}, errs.Decode("", "", "invalid JSON: %v", err)
	}
	if err := requireKeys(raw, "", "", "schema_version", "algorithm_version", "precision", "speed_of_sound_mm_s", "tolerance", "listener_rooms", "pairs"); err != nil {
		return model.Truth{}, err
	}
	if isJSONNull(raw["listener_rooms"]) {
		return model.Truth{}, errs.Decode("", "listener_rooms", "required field is null")
	}
	var tol map[string]json.RawMessage
	if err := json.Unmarshal(raw["tolerance"], &tol); err != nil {
		return model.Truth{}, errs.Decode("", "tolerance", "must be an object: %v", err)
	}
	if err := requireKeys(tol, "", "tolerance", "path_mm", "decay_ms", "early_energy"); err != nil {
		return model.Truth{}, err
	}
	var pairs []json.RawMessage
	if isJSONNull(raw["pairs"]) {
		return model.Truth{}, errs.Decode("", "pairs", "required field is null")
	}
	if err := json.Unmarshal(raw["pairs"], &pairs); err != nil {
		return model.Truth{}, errs.Decode("", "pairs", "must be an array: %v", err)
	}
	for i, p := range pairs {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(p, &obj); err != nil {
			return model.Truth{}, errs.Decode("", fmt.Sprintf("pairs[%d]", i), "must be an object")
		}
		room := ""
		if err := requireKeys(obj, room, fmt.Sprintf("pairs[%d]", i), "source_id", "listener_id", "reachable", "room_path", "door_path", "path_mm", "decay_ms", "early_energy"); err != nil {
			return model.Truth{}, err
		}
	}
	var truth model.Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		return model.Truth{}, errs.Decode("", "", "invalid truth JSON: %v", err)
	}
	if truth.SchemaVersion != model.TruthSchema {
		return model.Truth{}, errs.Decode("", "schema_version", "want %q got %q", model.TruthSchema, truth.SchemaVersion)
	}
	if truth.SpeedOfSoundMMPS != model.SpeedOfSoundMMPS {
		return model.Truth{}, errs.Decode("", "speed_of_sound_mm_s", "unsupported speed of sound %d", truth.SpeedOfSoundMMPS)
	}
	if truth.Precision != model.Precision {
		return model.Truth{}, errs.Decode("", "precision", "want %q got %q", model.Precision, truth.Precision)
	}
	return truth, nil
}

func checkRoomsRaw(raw json.RawMessage) error {
	if isJSONNull(raw) {
		return errs.Decode("", "rooms", "required field is null")
	}
	var rooms []json.RawMessage
	if err := json.Unmarshal(raw, &rooms); err != nil {
		return errs.Decode("", "rooms", "must be an array: %v", err)
	}
	seen := map[string]bool{}
	for i, item := range rooms {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			return errs.Decode(fmt.Sprintf("rooms[%d]", i), "rooms", "room must be an object")
		}
		id := jsonString(obj["id"])
		if id == "" {
			id = fmt.Sprintf("rooms[%d]", i)
		}
		if err := requireKeys(obj, id, fmt.Sprintf("rooms[%d]", i), "id", "rect", "params"); err != nil {
			return err
		}
		if seen[id] {
			return errs.Decode(id, "id", "duplicate room id")
		}
		seen[id] = true
		var rect map[string]json.RawMessage
		if err := json.Unmarshal(obj["rect"], &rect); err != nil {
			return errs.Decode(id, "rect", "must be an object")
		}
		if err := requireKeys(rect, id, "rect", "x", "y", "w", "h"); err != nil {
			return err
		}
		var params map[string]json.RawMessage
		if err := json.Unmarshal(obj["params"], &params); err != nil {
			return errs.Decode(id, "params", "must be an object")
		}
		if err := requireKeys(params, id, "params", "t60_ms", "early_density", "reverb_gain"); err != nil {
			return err
		}
	}
	return nil
}

func checkWallsRaw(raw json.RawMessage) error {
	if isJSONNull(raw) {
		return errs.Decode("", "walls", "required field is null")
	}
	var walls []json.RawMessage
	if err := json.Unmarshal(raw, &walls); err != nil {
		return errs.Decode("", "walls", "must be an array: %v", err)
	}
	for i, item := range walls {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			return errs.Decode("", fmt.Sprintf("walls[%d]", i), "wall must be an object")
		}
		id := jsonString(obj["id"])
		label := fmt.Sprintf("walls[%d]", i)
		if err := requireKeys(obj, "", label, "id", "start", "end", "rooms"); err != nil {
			return err
		}
		if id == "" {
			id = label
		}
		for _, key := range []string{"start", "end"} {
			var pt map[string]json.RawMessage
			if err := json.Unmarshal(obj[key], &pt); err != nil {
				return errs.Decode("", label+"."+key, "must be an object")
			}
			if err := requireKeys(pt, "", label+"."+key, "x", "y"); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkOpeningsRaw(raw json.RawMessage) error {
	if isJSONNull(raw) {
		return errs.Decode("", "openings", "required field is null")
	}
	var openings []json.RawMessage
	if err := json.Unmarshal(raw, &openings); err != nil {
		return errs.Decode("", "openings", "must be an array: %v", err)
	}
	for i, item := range openings {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			return errs.Decode("", fmt.Sprintf("openings[%d]", i), "opening must be an object")
		}
		label := fmt.Sprintf("openings[%d]", i)
		if err := requireKeys(obj, "", label, "id", "wall_id", "center", "width", "active"); err != nil {
			return err
		}
		var pt map[string]json.RawMessage
		if err := json.Unmarshal(obj["center"], &pt); err != nil {
			return errs.Decode("", label+".center", "must be an object")
		}
		if err := requireKeys(pt, "", label+".center", "x", "y"); err != nil {
			return err
		}
	}
	return nil
}

func checkPointsRaw(raw json.RawMessage, kind string) error {
	if isJSONNull(raw) {
		return errs.Decode("", kind+"s", "required field is null")
	}
	var points []json.RawMessage
	if err := json.Unmarshal(raw, &points); err != nil {
		return errs.Decode("", kind+"s", "must be an array: %v", err)
	}
	for i, item := range points {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			return errs.Decode("", fmt.Sprintf("%ss[%d]", kind, i), "%s must be an object", kind)
		}
		label := fmt.Sprintf("%ss[%d]", kind, i)
		if err := requireKeys(obj, "", label, "id", "pos"); err != nil {
			return err
		}
		var pt map[string]json.RawMessage
		if err := json.Unmarshal(obj["pos"], &pt); err != nil {
			return errs.Decode("", label+".pos", "must be an object")
		}
		if err := requireKeys(pt, "", label+".pos", "x", "y"); err != nil {
			return err
		}
	}
	return nil
}

func requireKeys(obj map[string]json.RawMessage, room, prefix string, keys ...string) error {
	for _, key := range keys {
		if value, ok := obj[key]; !ok || isJSONNull(value) {
			field := key
			if prefix != "" {
				field = prefix + "." + key
			}
			return errs.Decode(room, field, "required field is missing")
		}
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func jsonString(v json.RawMessage) string {
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
