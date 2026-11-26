package sampleio

import (
	"encoding/json"
	"os"
	"path/filepath"

	"iconflow/errs"
	"iconflow/generator"
	"iconflow/model"
	"iconflow/schemax"
)

type Set struct {
	Dir       string                           `json:"dir"`
	Documents map[string]string                `json:"documents"`
	Exports   map[string]*schemax.ExportResult `json:"-"`
}

func WriteSet(dir string, project *model.Project) (*Set, *errs.Error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.New(errs.EInternal, dir, "create sample directory: %v", err)
	}
	if err := generator.WritePNGs(dir, project); err != nil {
		return nil, errs.New(errs.EInternal, dir, "write PNG samples: %v", err)
	}
	set := &Set{Dir: dir, Documents: map[string]string{}, Exports: map[string]*schemax.ExportResult{}}
	versions := []string{model.SchemaV1, model.SchemaV2, model.SchemaV3}
	for _, version := range versions {
		export, exportErr := schemax.Export(project, version)
		if exportErr != nil {
			return nil, exportErr
		}
		filename := "project." + versionSuffix(version) + ".json"
		path := filepath.Join(dir, filename)
		if err := os.WriteFile(path, export.Data, 0o644); err != nil {
			return nil, errs.New(errs.EInternal, path, "write schema document: %v", err)
		}
		set.Documents[version] = path
		set.Exports[version] = export
	}
	truthData, err := json.MarshalIndent(project.Truth, "", "  ")
	if err != nil {
		return nil, errs.New(errs.EInternal, "$.truth", "marshal truth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), truthData, 0o644); err != nil {
		return nil, errs.New(errs.EInternal, "truth.json", "write truth: %v", err)
	}
	dirtyData, _ := json.MarshalIndent(project.Dirty, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "dirty-blocks.json"), dirtyData, 0o644)
	return set, nil
}

func versionSuffix(version string) string {
	switch version {
	case model.SchemaV1:
		return "v1"
	case model.SchemaV2:
		return "v2"
	default:
		return "v3"
	}
}

func LoadFile(path string) (*schemax.LoadResult, *errs.Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errs.New(errs.ESchema, path, "read schema file: %v", err)
	}
	return schemax.LoadDocument(data)
}
