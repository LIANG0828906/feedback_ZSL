package generator

import (
	"encoding/json"
	"os"
	"path/filepath"

	"maintenanceorchestrator/internal/domain"
	"maintenanceorchestrator/internal/schema"
	"maintenanceorchestrator/internal/truth"
)

type WrittenSample struct {
	Directory string `json:"directory"`
	V1Path    string `json:"v1_path"`
	V2Path    string `json:"v2_path"`
	V3Path    string `json:"v3_path"`
	TruthPath string `json:"truth_path"`
	ProjectID string `json:"project_id"`
}

func WriteSample(dir string, seed int64) (WrittenSample, domain.Project, truth.Truth, error) {
	p := Generate(seed)
	t := truth.Construction(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return WrittenSample{}, p, t, err
	}
	files := []struct {
		name    string
		version string
	}{
		{"project.v1.json", domain.SchemaV1},
		{"project.v2.json", domain.SchemaV2},
		{"project.v3.json", domain.SchemaV3},
	}
	out := WrittenSample{Directory: dir, ProjectID: p.ProjectID}
	for i, f := range files {
		data, err := schema.Encode(p, f.version)
		if err != nil {
			return out, p, t, err
		}
		data = append(data, '\n')
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return out, p, t, err
		}
		switch i {
		case 0:
			out.V1Path = path
		case 1:
			out.V2Path = path
		case 2:
			out.V3Path = path
		}
	}
	truthData, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return out, p, t, err
	}
	truthData = append(truthData, '\n')
	out.TruthPath = filepath.Join(dir, "truth.json")
	if err := os.WriteFile(out.TruthPath, truthData, 0o644); err != nil {
		return out, p, t, err
	}
	return out, p, t, nil
}
