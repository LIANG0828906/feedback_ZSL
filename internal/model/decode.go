package model

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func DecodeProject(data []byte, requireTruth bool) (*Project, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, SchemaError("project", "decode project JSON: %v", err)
	}
	required := []string{
		"schema_version", "seed", "bounds", "rooms", "walls", "doors",
		"sources", "listeners", "tolerance",
	}
	if requireTruth {
		required = append(required, "truth")
	}
	if err := requireKeys(root, "project", required...); err != nil {
		return nil, err
	}
	if err := requireObject(root["bounds"], "bounds", "x", "y", "w", "h"); err != nil {
		return nil, err
	}
	if err := requireObject(root["tolerance"], "tolerance", "path_length_micro", "curve_db_micro", "spl_db_micro"); err != nil {
		return nil, err
	}
	if err := requireObjectArray(root["rooms"], "rooms", "id", "rect", "rt60_ms", "early_reflection_density_milli", "reverb_gain_milli"); err != nil {
		return nil, err
	}
	var rooms []map[string]json.RawMessage
	if err := json.Unmarshal(root["rooms"], &rooms); err != nil {
		return nil, SchemaError("rooms", "decode rooms: %v", err)
	}
	for i, room := range rooms {
		if err := requireObject(room["rect"], fmt.Sprintf("rooms[%d].rect", i), "x", "y", "w", "h"); err != nil {
			return nil, err
		}
	}
	if err := requireObjectArray(root["walls"], "walls", "id", "x1", "y1", "x2", "y2", "room_a", "room_b"); err != nil {
		return nil, err
	}
	if err := requireObjectArray(root["doors"], "doors", "id", "wall_id", "start", "width"); err != nil {
		return nil, err
	}
	if err := requireObjectArray(root["sources"], "sources", "id", "x", "y"); err != nil {
		return nil, err
	}
	if err := requireObjectArray(root["listeners"], "listeners", "id", "x", "y"); err != nil {
		return nil, err
	}
	if truthRaw, ok := root["truth"]; ok && !bytes.Equal(bytes.TrimSpace(truthRaw), []byte("null")) {
		if err := validateTruthJSON(truthRaw); err != nil {
			return nil, err
		}
	}
	var project Project
	if err := json.Unmarshal(data, &project); err != nil {
		return nil, SchemaError("project", "decode project: %v", err)
	}
	if requireTruth && project.Truth == nil {
		return nil, SchemaError("truth", "truth checklist is required")
	}
	return &project, nil
}

func validateTruthJSON(raw json.RawMessage) error {
	var truth map[string]json.RawMessage
	if err := json.Unmarshal(raw, &truth); err != nil {
		return SchemaError("truth", "decode truth: %v", err)
	}
	if err := requireKeys(truth, "truth", "algorithm_version", "project_fingerprint", "curve_samples", "curve_step_micros", "entries"); err != nil {
		return err
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(truth["entries"], &entries); err != nil {
		return SchemaError("truth.entries", "decode truth entries: %v", err)
	}
	for i, entry := range entries {
		path := fmt.Sprintf("truth.entries[%d]", i)
		if err := requireKeys(entry, path,
			"source_id", "listener_id", "listener_room_id", "reachable",
			"room_chain", "door_chain", "path_length_micro", "spl_db_micro", "occlusion_db_micro"); err != nil {
			return err
		}
		var reachable bool
		if err := json.Unmarshal(entry["reachable"], &reachable); err != nil {
			return SchemaError(path+".reachable", "reachable must be a boolean")
		}
		if reachable {
			if err := requireKeys(entry, path, "segments", "curve_db_micro"); err != nil {
				return err
			}
			var segments []map[string]json.RawMessage
			if err := json.Unmarshal(entry["segments"], &segments); err != nil {
				return SchemaError(path+".segments", "decode segments: %v", err)
			}
			for j, segment := range segments {
				if err := requireKeys(segment, fmt.Sprintf("%s.segments[%d]", path, j), "room_id", "from", "to", "length_micro"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func requireObject(raw json.RawMessage, path string, keys ...string) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return SchemaError(path, "%s must be an object", path)
	}
	return requireKeys(obj, path, keys...)
}

func requireObjectArray(raw json.RawMessage, path string, keys ...string) error {
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &objects); err != nil {
		return SchemaError(path, "%s must be an array of objects", path)
	}
	for i, object := range objects {
		if err := requireKeys(object, fmt.Sprintf("%s[%d]", path, i), keys...); err != nil {
			return err
		}
	}
	return nil
}

func requireKeys(obj map[string]json.RawMessage, path string, keys ...string) error {
	for _, key := range keys {
		if _, ok := obj[key]; !ok {
			return SchemaError(path+"."+key, "required field %s is missing", key)
		}
	}
	return nil
}
