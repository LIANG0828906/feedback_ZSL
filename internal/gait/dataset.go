package gait

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const DatasetSchemaVersion = "1.0"

type Dataset struct {
	SchemaVersion string           `json:"schema_version"`
	Sources       []SourceLoop     `json:"sources"`
	Phases        []PhaseTrack     `json:"phases"`
	Targets       []TargetSkeleton `json:"targets"`
	Truths        []Truth          `json:"truths"`
	Params        Params           `json:"params"`
}

type sourceDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Sources       []SourceLoop `json:"sources"`
}

type phaseDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Phases        []PhaseTrack `json:"phases"`
}

type targetDocument struct {
	SchemaVersion string           `json:"schema_version"`
	Targets       []TargetSkeleton `json:"targets"`
}

type truthDocument struct {
	SchemaVersion string  `json:"schema_version"`
	Truths        []Truth `json:"truths"`
}

func WriteDataset(dir string, ds Dataset) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	docs := []struct {
		name string
		doc  any
	}{
		{"sources.json", sourceDocument{SchemaVersion: ds.SchemaVersion, Sources: ds.Sources}},
		{"phases.json", phaseDocument{SchemaVersion: ds.SchemaVersion, Phases: ds.Phases}},
		{"targets.json", targetDocument{SchemaVersion: ds.SchemaVersion, Targets: ds.Targets}},
		{"truths.json", truthDocument{SchemaVersion: ds.SchemaVersion, Truths: ds.Truths}},
		{"params.json", ds.Params},
	}
	for _, item := range docs {
		data, err := json.MarshalIndent(item.doc, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if err := atomicWriteFile(filepath.Join(dir, item.name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func LoadDataset(dir string) (Dataset, error) {
	var ds Dataset
	var src sourceDocument
	if err := readJSON(filepath.Join(dir, "sources.json"), &src); err != nil {
		return ds, err
	}
	var phases phaseDocument
	if err := readJSON(filepath.Join(dir, "phases.json"), &phases); err != nil {
		return ds, err
	}
	var targets targetDocument
	if err := readJSON(filepath.Join(dir, "targets.json"), &targets); err != nil {
		return ds, err
	}
	var truths truthDocument
	if err := readJSON(filepath.Join(dir, "truths.json"), &truths); err != nil {
		return ds, err
	}
	var params Params
	if err := readJSON(filepath.Join(dir, "params.json"), &params); err != nil {
		return ds, err
	}
	if src.SchemaVersion != DatasetSchemaVersion {
		return ds, appErr(CodeSchema, fmt.Sprintf("sources schema_version must be %q", DatasetSchemaVersion))
	}
	if phases.SchemaVersion != DatasetSchemaVersion {
		return ds, appErr(CodeSchema, fmt.Sprintf("phases schema_version must be %q", DatasetSchemaVersion))
	}
	if targets.SchemaVersion != DatasetSchemaVersion {
		return ds, appErr(CodeSchema, fmt.Sprintf("targets schema_version must be %q", DatasetSchemaVersion))
	}
	if truths.SchemaVersion != DatasetSchemaVersion {
		return ds, appErr(CodeSchema, fmt.Sprintf("truths schema_version must be %q", DatasetSchemaVersion))
	}
	ds = Dataset{
		SchemaVersion: DatasetSchemaVersion,
		Sources:       src.Sources,
		Phases:        phases.Phases,
		Targets:       targets.Targets,
		Truths:        truths.Truths,
		Params:        params,
	}
	if err := ValidateDataset(ds); err != nil {
		return Dataset{}, err
	}
	return ds, nil
}

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	ok = true
	return nil
}

func SelectSource(ds Dataset, id string) (SourceLoop, PhaseTrack, Truth, error) {
	for _, source := range ds.Sources {
		if source.ID == id {
			for _, phases := range ds.Phases {
				if phases.SourceID == id {
					for _, truth := range ds.Truths {
						if truth.SourceID == id {
							return source, phases, truth, nil
						}
					}
					return SourceLoop{}, PhaseTrack{}, Truth{}, errAt(CodeSchema, "truth for source is missing", 0)
				}
			}
			return SourceLoop{}, PhaseTrack{}, Truth{}, errAt(CodeSchema, "phase track for source is missing", 0)
		}
	}
	return SourceLoop{}, PhaseTrack{}, Truth{}, appErr(CodeSchema, fmt.Sprintf("source %q not found", id))
}

func SelectTarget(ds Dataset, id string) (TargetSkeleton, error) {
	for _, target := range ds.Targets {
		if target.ID == id {
			return target, nil
		}
	}
	return TargetSkeleton{}, appErr(CodeSchema, fmt.Sprintf("target %q not found", id))
}
