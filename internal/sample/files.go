package sample

import (
	"encoding/json"
	"os"
	"path/filepath"

	"sre-timeline/internal/model"
	"sre-timeline/internal/schemaio"
)

type WrittenFiles struct {
	Directory string `json:"directory"`
	V1        string `json:"v1"`
	V2        string `json:"v2"`
	V3        string `json:"v3"`
	Truth     string `json:"truth"`
}

func WriteWorkspace(dir string, seed int64) (*model.Project, *WrittenFiles, error) {
	p := Generate(seed)
	if _, err := BuildTruth(p); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	files := &WrittenFiles{Directory: dir}
	write := func(name string, data []byte) (string, error) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
		return path, nil
	}
	for _, item := range []struct {
		version string
		target  *string
		name    string
	}{
		{model.SchemaV1, &files.V1, "review-project.v1.json"},
		{model.SchemaV2, &files.V2, "review-project.v2.json"},
		{model.SchemaV3, &files.V3, "review-project.v3.json"},
	} {
		data, err := schemaio.Encode(p, item.version)
		if err != nil {
			return nil, nil, err
		}
		path, err := write(item.name, data)
		if err != nil {
			return nil, nil, err
		}
		*item.target = path
	}
	truthData, err := json.MarshalIndent(p.Truth, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	files.Truth, err = write("expected-truth.json", truthData)
	if err != nil {
		return nil, nil, err
	}
	return p, files, nil
}
