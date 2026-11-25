package ocrflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type SampleFile struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type RecipeManifest struct {
	Schema        string `json:"schema"`
	SourceVersion string `json:"source_version"`
	Path          string `json:"path"`
	ParamVersion  string `json:"param_version"`
}

type SampleManifest struct {
	Seed      int64            `json:"seed"`
	Generator string           `json:"generator"`
	Width     int              `json:"width"`
	Height    int              `json:"height"`
	Shots     []*Screenshot    `json:"shots"`
	Files     []SampleFile     `json:"files"`
	Recipes   []RecipeManifest `json:"recipes"`
	TruthPath string           `json:"truth_path"`
}

func SampleRecipe() *Recipe {
	r := DefaultRecipe()
	r.Items = []RecipeItem{
		{
			ID: "score-panel", Text: "SCORE", ThresholdBPS: 7200, CharCount: 5,
			Box:                  Quad{{X: 16, Y: 24}, {X: 176, Y: 24}, {X: 176, Y: 80}, {X: 16, Y: 80}},
			CoordinateExpression: "absolute_pixels",
		},
		{
			ID: "rank-panel", Text: "RANK", ThresholdBPS: 6835, CharCount: 4,
			Box:                  Quad{{X: 21, Y: 33}, {X: 171, Y: 33}, {X: 171, Y: 87}, {X: 21, Y: 87}},
			CoordinateExpression: "absolute_pixels",
		},
	}
	r.ParamVersion = ComputeParamVersion(r)
	return r
}

func WriteSampleSet(dir string, seed int64) (*SampleManifest, error) {
	if seed == 0 {
		seed = FixedSeed
	}
	shots, err := GenerateSampleSet(seed)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "shots"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "recipes"), 0o755); err != nil {
		return nil, err
	}
	manifest := &SampleManifest{Seed: seed, Generator: "ocrflow.deterministic-v1", Width: 720, Height: 420, Shots: shots, TruthPath: "truth.json"}
	addFile := func(rel string, data []byte) error {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, SampleFile{Path: rel, Bytes: len(data), SHA256: HashBytes(data)})
		return nil
	}

	truthData, err := json.MarshalIndent(map[string]any{"seed": seed, "shots": shots}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := addFile("truth.json", append(truthData, '\n')); err != nil {
		return nil, err
	}

	for _, shot := range shots {
		pngData, err := EncodePNG(shot.RGBA)
		if err != nil {
			return nil, err
		}
		rel := filepath.ToSlash(filepath.Join("shots", shot.ShotID+".png"))
		if err := addFile(rel, pngData); err != nil {
			return nil, err
		}
	}

	recipe := SampleRecipe()
	schemas := []string{SchemaV1, SchemaV2, SchemaV3}
	for _, schema := range schemas {
		data, err := EncodeRecipeDocument(recipe, schema)
		if err != nil {
			return nil, err
		}
		version := schemaVersionName(schema)
		rel := filepath.ToSlash(filepath.Join("recipes", "recipe-"+version+".json"))
		if err := addFile(rel, data); err != nil {
			return nil, err
		}
		loaded, err := LoadRecipeDocument(data)
		if err != nil {
			return nil, fmt.Errorf("generated recipe %s did not reload: %w", version, err)
		}
		manifest.Recipes = append(manifest.Recipes, RecipeManifest{Schema: schema, SourceVersion: version, Path: rel, ParamVersion: loaded.ParamVersion})
	}

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(manifestData, '\n'), 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func VerifySampleSet(dir string, seed int64) (*SampleManifest, error) {
	if seed == 0 {
		seed = FixedSeed
	}
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest SampleManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, err
	}
	regenerated, err := GenerateSampleSet(seed)
	if err != nil {
		return nil, err
	}
	if len(regenerated) != len(manifest.Shots) {
		return nil, fmt.Errorf("manifest shot count %d != regenerated %d", len(manifest.Shots), len(regenerated))
	}
	for i := range regenerated {
		if regenerated[i].ContentHash != manifest.Shots[i].ContentHash {
			return nil, fmt.Errorf("shot %s content hash is not reproducible", regenerated[i].ShotID)
		}
		if len(regenerated[i].Lines) != len(manifest.Shots[i].Lines) {
			return nil, fmt.Errorf("shot %s truth line count changed", regenerated[i].ShotID)
		}
	}
	for _, f := range manifest.Files {
		data, err := os.ReadFile(filepath.Join(dir, f.Path))
		if err != nil {
			return nil, err
		}
		if HashBytes(data) != f.SHA256 {
			return nil, fmt.Errorf("sample file %s hash mismatch", f.Path)
		}
	}
	loadedCount := 0
	for _, rf := range manifest.Recipes {
		data, err := os.ReadFile(filepath.Join(dir, rf.Path))
		if err != nil {
			return nil, err
		}
		r, err := LoadRecipeDocument(data)
		if err != nil {
			return nil, err
		}
		if r.SourceVersion != rf.SourceVersion {
			return nil, fmt.Errorf("recipe %s source version mismatch", rf.Path)
		}
		loadedCount++
	}
	if loadedCount < 3 {
		return nil, fmt.Errorf("expected 3 recipe schemas, loaded %d", loadedCount)
	}
	return &manifest, nil
}
