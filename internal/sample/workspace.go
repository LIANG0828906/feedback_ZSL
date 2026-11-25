package sample

import (
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"

	"maskqa/internal/model"
	"maskqa/internal/schemaio"
)

type Manifest struct {
	Seed     int64             `json:"seed"`
	Projects []ProjectManifest `json:"projects"`
}

type ProjectManifest struct {
	ID           string `json:"id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Image        string `json:"image"`
	CandidatePNG string `json:"candidate_png"`
	ReferencePNG string `json:"reference_png"`
	InternalJSON string `json:"internal_json"`
	TruthJSON    string `json:"truth_json"`
	SchemaV1     string `json:"schema_v1"`
	SchemaV2     string `json:"schema_v2"`
	SchemaV3     string `json:"schema_v3"`
	StrokeCount  int    `json:"stroke_count"`
	DomainCount  int    `json:"domain_count"`
}

func WriteWorkspace(root string, seed int64, assets []Asset) (*Manifest, error) {
	if err := os.MkdirAll(filepath.Join(root, "samples"), 0o755); err != nil {
		return nil, err
	}
	manifest := &Manifest{Seed: seed}
	for _, asset := range assets {
		p := asset.Project
		dir := filepath.Join(root, "samples", p.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		paths := ProjectManifest{
			ID: p.ID, Width: p.Width, Height: p.Height, StrokeCount: len(p.Strokes), DomainCount: p.Truth.DomainCount,
			Image:        filepath.ToSlash(filepath.Join("samples", p.ID, "image.png")),
			CandidatePNG: filepath.ToSlash(filepath.Join("samples", p.ID, "candidate.png")),
			ReferencePNG: filepath.ToSlash(filepath.Join("samples", p.ID, "reference.png")),
			InternalJSON: filepath.ToSlash(filepath.Join("samples", p.ID, "project.internal.json")),
			TruthJSON:    filepath.ToSlash(filepath.Join("samples", p.ID, "truth.json")),
			SchemaV1:     filepath.ToSlash(filepath.Join("samples", p.ID, "project.v1.json")),
			SchemaV2:     filepath.ToSlash(filepath.Join("samples", p.ID, "project.v2.json")),
			SchemaV3:     filepath.ToSlash(filepath.Join("samples", p.ID, "project.v3.json")),
		}
		if err := writePNG(filepath.Join(root, paths.Image), asset.Image); err != nil {
			return nil, err
		}
		if err := writeGrayMask(filepath.Join(root, paths.CandidatePNG), p.Candidate); err != nil {
			return nil, err
		}
		if err := writeGrayMask(filepath.Join(root, paths.ReferencePNG), p.Reference); err != nil {
			return nil, err
		}
		if err := writeJSON(filepath.Join(root, paths.InternalJSON), p); err != nil {
			return nil, err
		}
		if err := writeJSON(filepath.Join(root, paths.TruthJSON), p.Truth); err != nil {
			return nil, err
		}
		eng := &schemaio.Engineering{
			SchemaVersion: model.SchemaInternal, ProjectID: p.ID, Seed: p.Seed,
			Width: p.Width, Height: p.Height, ChunkSize: p.ChunkSize, Strokes: p.Strokes,
		}
		for _, item := range []struct {
			version string
			path    string
		}{{schemaio.V1, paths.SchemaV1}, {schemaio.V2, paths.SchemaV2}, {schemaio.V3, paths.SchemaV3}} {
			raw, err := schemaio.MarshalDocument(eng, item.version)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(root, item.path), raw, 0o644); err != nil {
				return nil, err
			}
		}
		manifest.Projects = append(manifest.Projects, paths)
	}
	if err := writeJSON(filepath.Join(root, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func LoadProject(path string) (*model.Project, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p model.Project
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func writeGrayMask(path string, m model.Mask) error {
	img := image.NewGray(image.Rect(0, 0, m.Width, m.Height))
	copy(img.Pix, m.Pix)
	return writePNG(path, img)
}

func hashStrokeForGenerator(s model.Stroke) string {
	return schemaio.StrokeHash(s)
}
