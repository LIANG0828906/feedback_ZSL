package sample

import (
	"encoding/json"
	"os"
	"path/filepath"

	"cadpipe/internal/cad"
)

type Manifest struct {
	Seed              int64             `json:"seed"`
	SchemaVersions    map[string]string `json:"schema_versions"`
	Files             map[string]string `json:"files"`
	TranscriptEntries int               `json:"transcript_entries"`
}

func GenerateFiles(out string, seed int64) (*Bundle, error) {
	bundle, err := Generate(seed)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"scene.v1.json":           bundle.SchemaV1,
		"scene.v2.json":           bundle.SchemaV2,
		"scene.v3.json":           bundle.SchemaV3,
		"truth.json":              marshalOrPanic(bundle.Truth),
		"subdivision_meshes.json": marshalOrPanic(bundle.Meshes),
		"expected_tiles.json":     marshalOrPanic(bundle.Tiles),
		"camera.json":             marshalOrPanic(bundle.Camera),
		"transcript.v1.json":      marshalOrPanic(bundle.TranscriptV1),
	}
	manifest := Manifest{
		Seed:              seed,
		SchemaVersions:    map[string]string{"v1": "cad.scene/v1", "v2": "cad.scene/v2", "v3": "cad.scene/v3"},
		Files:             make(map[string]string, len(files)),
		TranscriptEntries: len(bundle.TranscriptV1.Entries),
	}
	for name, data := range files {
		path := filepath.Join(out, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
		manifest.Files[name] = cad.SHA256Hex(data)
	}
	manifestData := marshalOrPanic(manifest)
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), manifestData, 0o644); err != nil {
		return nil, err
	}
	return bundle, nil
}

func marshalOrPanic(v any) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(data, '\n')
}
