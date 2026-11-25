package formats

import (
	"encoding/json"
	"os"
	"path/filepath"

	"replayaudit/internal/errs"
	"replayaudit/internal/model"
)

// Package is one replay directory: parameters, three source formats,
// the merged timeline and optional parser truth.
type Package struct {
	Dir     string
	Params  model.Params
	Replay  *model.Replay
	Truth   *model.Truth
	Partial map[int]*model.Replay
}

// LoadPackage loads params.json, all three format files and truth.json.
func LoadPackage(dir string) (*Package, error) {
	params, err := LoadParams(dir)
	if err != nil {
		return nil, err
	}
	p1, _, err := LoadFile(filepath.Join(dir, File1), params)
	if err != nil {
		return nil, err
	}
	p2, _, err := LoadFile(filepath.Join(dir, File2), params)
	if err != nil {
		return nil, err
	}
	p3, _, err := LoadFile(filepath.Join(dir, File3), params)
	if err != nil {
		return nil, err
	}
	merged, err := Merge(p1, p2, p3)
	if err != nil {
		return nil, err
	}
	pkg := &Package{
		Dir: dir, Params: params, Replay: merged,
		Partial: map[int]*model.Replay{1: p1, 2: p2, 3: p3},
	}
	truthPath := filepath.Join(dir, FileTruth)
	if data, err := os.ReadFile(truthPath); err == nil {
		var t model.Truth
		if jerr := json.Unmarshal(data, &t); jerr != nil {
			return nil, errs.New(errs.EFormat, truthPath, "", "invalid truth document: %v", jerr)
		}
		pkg.Truth = &t
	} else if !os.IsNotExist(err) {
		return nil, errs.New(errs.EFormat, truthPath, "", "read failed: %v", err)
	}
	return pkg, nil
}

// WriteJSON is a small helper used by the deterministic generator.
func WriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
