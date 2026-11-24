package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"volumediag/internal/diag"
)

// LoadBundle reads scene.json and truth.json from a directory (or accepts a
// path to scene.json directly). Structural checks are intentionally strict so
// callers get stable diagnostic codes instead of zero-valued model fields.
func LoadBundle(sceneDir string) (*Bundle, error) {
	scenePath, truthPath, err := resolvePaths(sceneDir)
	if err != nil {
		return nil, err
	}
	sceneBytes, readErr := os.ReadFile(scenePath)
	if readErr != nil {
		return nil, diag.IO("read scene file", readErr)
	}
	truthBytes, readErr := os.ReadFile(truthPath)
	if readErr != nil {
		return nil, diag.IO("read truth file", readErr)
	}

	scene, err := DecodeScene(sceneBytes)
	if err != nil {
		return nil, err
	}
	truth, err := DecodeTruth(truthBytes, scene)
	if err != nil {
		return nil, err
	}
	sceneHash := scene.ComputeHash()
	sourceHash := HashSources(sceneBytes, truthBytes)

	manifestPath := filepath.Join(filepath.Dir(scenePath), "manifest.json")
	if b, e := os.ReadFile(manifestPath); e == nil {
		var m SourceManifest
		if decodeErr := json.Unmarshal(b, &m); decodeErr != nil {
			return nil, diag.Decode("manifest", "invalid manifest JSON", decodeErr)
		}
		if m.GeneratorID != "" && m.GeneratorID != GeneratorID {
			return nil, diag.Decode("manifest.generator_id", "unsupported generator identity", nil)
		}
		if m.Seed != 0 && m.Seed != scene.Seed {
			return nil, diag.Decode("manifest.seed", "manifest seed does not match scene", nil)
		}
		if m.SourceHash != "" && m.SourceHash != sourceHash {
			return nil, diag.Decode("manifest.source_hash", "source manifest hash mismatch", nil)
		}
		if m.SceneHash != "" && m.SceneHash != sceneHash {
			return nil, diag.Decode("manifest.scene_hash", "scene hash mismatch", nil)
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, diag.IO("read manifest file", e)
	}

	return &Bundle{
		Scene:      scene,
		Truth:      truth,
		SourceHash: sourceHash,
		SceneHash:  sceneHash,
		Dir:        filepath.Dir(scenePath),
	}, nil
}

func resolvePaths(path string) (string, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", "", diag.IO("locate scene input", err)
	}
	if info.IsDir() {
		return filepath.Join(path, "scene.json"), filepath.Join(path, "truth.json"), nil
	}
	if strings.EqualFold(filepath.Base(path), "scene.json") {
		return path, filepath.Join(filepath.Dir(path), "truth.json"), nil
	}
	return "", "", diag.IO("locate scene input", fmt.Errorf("expected scene.json or a directory"))
}

func DecodeScene(raw []byte) (*Scene, error) {
	root, err := objectFields(raw, "scene")
	if err != nil {
		return nil, err
	}
	if err := requireFields(root, "scene", "", "format_version", "seed", "viewport", "block_size",
		"active_camera_id", "camera_path", "geometry", "lights", "medium"); err != nil {
		return nil, err
	}
	var scene Scene
	if err := json.Unmarshal(raw, &scene); err != nil {
		return nil, diag.Decode("scene", "cannot decode scene JSON", err)
	}
	if err := checkSceneObjectFields(root, &scene); err != nil {
		return nil, err
	}
	if scene.FormatVersion != SceneFormat {
		return nil, diag.Schema("scene.format_version", "unsupported scene format")
	}
	if err := validateScene(&scene); err != nil {
		return nil, err
	}
	return &scene, nil
}

func DecodeTruth(raw []byte, scene *Scene) (*Truth, error) {
	root, err := objectFields(raw, "truth")
	if err != nil {
		return nil, err
	}
	if err := requireFields(root, "truth", "", "format_version", "tolerance", "rays", "pixels"); err != nil {
		return nil, err
	}
	var truth Truth
	if err := json.Unmarshal(raw, &truth); err != nil {
		return nil, diag.Decode("truth", "cannot decode truth JSON", err)
	}
	if truth.FormatVersion != TruthFormat {
		return nil, diag.Schema("truth.format_version", "unsupported truth format")
	}
	if err := checkTruthObjectFields(root, &truth); err != nil {
		return nil, err
	}
	if err := validateTruth(&truth, scene); err != nil {
		return nil, err
	}
	return &truth, nil
}

func objectFields(raw []byte, path string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, diag.Decode(path, "malformed JSON object", err)
	}
	if obj == nil {
		return nil, diag.Decode(path, "expected JSON object", nil)
	}
	return obj, nil
}

func requireFields(obj map[string]json.RawMessage, path, lightID string, fields ...string) *diag.Error {
	for _, f := range fields {
		if _, ok := obj[f]; !ok {
			return &diag.Error{
				Code:    diag.ESchema,
				LightID: lightID,
				Field:   path + "." + f,
				Message: "required field is missing",
			}
		}
	}
	return nil
}

func arrayObjects(raw json.RawMessage, path string) ([]map[string]json.RawMessage, *diag.Error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, diag.Decode(path, "expected array", err)
	}
	out := make([]map[string]json.RawMessage, len(items))
	for i, item := range items {
		obj, err := objectFields(item, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			if de := diag.As(err); de != nil {
				return nil, de
			}
			return nil, diag.Decode(path, "invalid array item", err)
		}
		out[i] = obj
	}
	return out, nil
}

func checkSceneObjectFields(root map[string]json.RawMessage, scene *Scene) error {
	viewport, err := objectFields(root["viewport"], "scene.viewport")
	if err != nil {
		return err
	}
	if err := requireFields(viewport, "scene.viewport", "", "width", "height"); err != nil {
		return err
	}
	block, err := objectFields(root["block_size"], "scene.block_size")
	if err != nil {
		return err
	}
	if err := requireFields(block, "scene.block_size", "", "width", "height"); err != nil {
		return err
	}

	cameras, diagErr := arrayObjects(root["camera_path"], "scene.camera_path")
	if diagErr != nil {
		return diagErr
	}
	for i, obj := range cameras {
		if err := requireFields(obj, fmt.Sprintf("scene.camera_path[%d]", i), "",
			"id", "position", "target", "up", "fov_y_deg", "near", "far"); err != nil {
			return err
		}
	}

	meshes, diagErr := arrayObjects(root["geometry"], "scene.geometry")
	if diagErr != nil {
		return diagErr
	}
	for i, obj := range meshes {
		if err := requireFields(obj, fmt.Sprintf("scene.geometry[%d]", i), "",
			"id", "origin", "axis_u", "axis_v", "normal", "columns", "rows", "depths", "color"); err != nil {
			return err
		}
	}

	lights, diagErr := arrayObjects(root["lights"], "scene.lights")
	if diagErr != nil {
		return diagErr
	}
	for i, obj := range lights {
		lightID := rawString(obj["id"])
		path := fmt.Sprintf("scene.lights[%d]", i)
		if err := requireFields(obj, path, lightID,
			"id", "type", "position", "direction", "intensity", "radius", "anisotropy", "occluder_ids"); err != nil {
			return err
		}
		switch scene.Lights[i].Type {
		case "spot":
			if err := requireFields(obj, path, lightID, "cone_inner_deg", "cone_outer_deg"); err != nil {
				return err
			}
		case "area":
			if err := requireFields(obj, path, lightID, "half_extents"); err != nil {
				return err
			}
		}
	}

	medium, err := objectFields(root["medium"], "scene.medium")
	if err != nil {
		return err
	}
	if err := requireFields(medium, "scene.medium", "", "extinction", "scattering", "phase_model", "phase_g"); err != nil {
		return err
	}
	return nil
}

func checkTruthObjectFields(root map[string]json.RawMessage, truth *Truth) error {
	tol, err := objectFields(root["tolerance"], "truth.tolerance")
	if err != nil {
		return err
	}
	if err := requireFields(tol, "truth.tolerance", "", "transmittance_abs", "scatter_abs", "pixel_abs", "relative"); err != nil {
		return err
	}
	rays, diagErr := arrayObjects(root["rays"], "truth.rays")
	if diagErr != nil {
		return diagErr
	}
	for i, obj := range rays {
		if err := requireFields(obj, fmt.Sprintf("truth.rays[%d]", i), "",
			"id", "camera_id", "pixel_x", "pixel_y", "origin", "direction",
			"t_near", "t_far", "transmittance", "scatter"); err != nil {
			return err
		}
	}
	pixels, diagErr := arrayObjects(root["pixels"], "truth.pixels")
	if diagErr != nil {
		return diagErr
	}
	for i, obj := range pixels {
		if err := requireFields(obj, fmt.Sprintf("truth.pixels[%d]", i), "",
			"camera_id", "x", "y", "volume"); err != nil {
			return err
		}
	}
	return nil
}

func rawString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}
