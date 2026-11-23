package workbench

import (
	"encoding/json"
	"os"
)

// LoadScene decodes a serialized scene bundle and applies the full metadata,
// band, exposure-order and white-point validation contract before returning.
func LoadScene(data []byte) (*Scene, error) {
	var scene Scene
	if err := json.Unmarshal(data, &scene); err != nil {
		return nil, fieldErr(CodeMeta, "scene", err.Error(), "valid scene JSON", "scene metadata cannot be decoded")
	}
	if scene.ID == "" {
		return nil, fieldErr(CodeMeta, "id", scene.ID, "non-empty", "scene identifier is missing")
	}
	if err := ValidateScene(&scene); err != nil {
		return nil, err
	}
	return &scene, nil
}

func LoadSceneFile(path string) (*Scene, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, wbErr(CodeIO, "read scene bundle: %v", err)
	}
	return LoadScene(data)
}

func SaveSceneFile(path string, scene *Scene) error {
	if err := ValidateScene(scene); err != nil {
		return err
	}
	data, err := json.Marshal(scene)
	if err != nil {
		return wbErr(CodeIO, "encode scene bundle: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return wbErr(CodeIO, "write scene bundle: %v", err)
	}
	return nil
}
