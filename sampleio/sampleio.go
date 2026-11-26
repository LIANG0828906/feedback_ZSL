// Package sampleio writes and reads the deterministic local sample corpus.
package sampleio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"quotakernel/generate"
	"quotakernel/model"
	"quotakernel/schema"
)

type Manifest struct {
	Seed     int64    `json:"seed"`
	Projects []string `json:"projects"`
}

type LoadedProject struct {
	Name  string
	V1    *schema.V1Document
	V2    *schema.V2Document
	V3    *schema.V3Document
	Truth *model.Truth
}

type LoadedBundle struct {
	Seed     int64
	Projects []*LoadedProject
	Critical []model.ConversionDiagnostic
}

func WriteBundle(dir string, bundle *generate.Bundle) error {
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "truth"), 0o755); err != nil {
		return err
	}
	manifest := Manifest{Seed: bundle.Seed}
	for _, p := range bundle.Projects {
		manifest.Projects = append(manifest.Projects, p.Name)
		if err := writeJSON(filepath.Join(dir, "projects", p.Name+".v1.json"), p.V1); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, "projects", p.Name+".v2.json"), p.V2); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, "projects", p.Name+".v3.json"), p.V3); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, "truth", p.Name+".truth.json"), p.Truth); err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(dir, "critical-conversions.json"), bundle.Critical); err != nil {
		return err
	}
	for _, p := range bundle.Projects {
		if p.Name != "cluster-main" {
			continue
		}
		degraded := generate.V3WithoutBurst(p.Canonical, "t-alpha")
		if err := os.MkdirAll(filepath.Join(dir, "degradation"), 0o755); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, "degradation", "cluster-main.missing-burst.v3.json"), degraded); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(dir, "manifest.json"), manifest)
}

func LoadBundle(dir string) (*LoadedBundle, error) {
	var manifest Manifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &manifest); err != nil {
		return nil, err
	}
	loaded := &LoadedBundle{Seed: manifest.Seed}
	for _, name := range manifest.Projects {
		lp := &LoadedProject{Name: name}
		lp.V1 = &schema.V1Document{}
		lp.V2 = &schema.V2Document{}
		lp.V3 = &schema.V3Document{}
		if err := readJSON(filepath.Join(dir, "projects", name+".v1.json"), lp.V1); err != nil {
			return nil, err
		}
		if err := readJSON(filepath.Join(dir, "projects", name+".v2.json"), lp.V2); err != nil {
			return nil, err
		}
		if err := readJSON(filepath.Join(dir, "projects", name+".v3.json"), lp.V3); err != nil {
			return nil, err
		}
		lp.Truth = &model.Truth{}
		if err := readJSON(filepath.Join(dir, "truth", name+".truth.json"), lp.Truth); err != nil {
			return nil, err
		}
		loaded.Projects = append(loaded.Projects, lp)
	}
	if err := readJSON(filepath.Join(dir, "critical-conversions.json"), &loaded.Critical); err != nil {
		return nil, err
	}
	return loaded, nil
}

func LoadVersion(dir, name string, version model.SchemaVersion) (*schema.LoadResult, error) {
	path := filepath.Join(dir, "projects", fmt.Sprintf("%s.%s.json", name, version))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return schema.LoadJSON(version, data)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
