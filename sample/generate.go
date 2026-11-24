package sample

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"economycore/engine"
	"economycore/format"
	"economycore/model"
)

// Truth is the deterministic per-cycle oracle.
type Truth struct {
	Seed           int64                  `json:"seed"`
	ContentHash    string                 `json:"contentHash"`
	KeyCycles      []int64                `json:"keyCycles"`
	Cycles         []engine.CycleSnapshot `json:"cycles"`
	OverflowTotals map[string]int64       `json:"overflowTotals"`
}

// Manifest describes one generated sample set.
type Manifest struct {
	Seed        int64                              `json:"seed"`
	ContentHash string                             `json:"contentHash"`
	KeyCycles   []int64                            `json:"keyCycles"`
	Files       map[string]string                  `json:"files"`
	Upgrades    map[string][]model.ConversionTrace `json:"upgrades"`
}

// Generate writes v1/v2/v3 project documents, their upgrade traces and truth
// into dir.
func Generate(ctx context.Context, dir string, seed int64) (*Manifest, error) {
	m := Scenario(seed)
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.NewError(model.EIO, dir, err.Error()).WithCause(err)
	}
	hash, err := m.ContentHash()
	if err != nil {
		return nil, err
	}
	manifest := &Manifest{
		Seed:        seed,
		ContentHash: hash,
		KeyCycles:   []int64{3, 7, 11},
		Files:       make(map[string]string),
		Upgrades:    make(map[string][]model.ConversionTrace),
	}
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		result, err := format.Export(m, version, format.ExportOptions{})
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", version, err)
		}
		subdir := filepath.Join(dir, version)
		if err := os.MkdirAll(subdir, 0o755); err != nil {
			return nil, model.NewError(model.EIO, subdir, err.Error()).WithCause(err)
		}
		path := filepath.Join(subdir, "project.json")
		data := append(result.Data, '\n')
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, model.NewError(model.EIO, path, err.Error()).WithCause(err)
		}
		manifest.Files[version] = relativeFile(dir, path, data)
		loaded, err := format.Load(data)
		if err != nil {
			return nil, fmt.Errorf("reload %s: %w", version, err)
		}
		_, traces, err := format.Upgrade(loaded, model.SchemaV3)
		if err != nil {
			return nil, err
		}
		if traces == nil {
			traces = []model.ConversionTrace{}
		}
		manifest.Upgrades[version] = traces
	}

	eng, err := engine.New(m, engine.Options{})
	if err != nil {
		return nil, err
	}
	run, err := eng.RunFull(ctx)
	if err != nil {
		return nil, err
	}
	truth := Truth{
		Seed:           seed,
		ContentHash:    hash,
		KeyCycles:      []int64{3, 7, 11},
		Cycles:         run.Cycles,
		OverflowTotals: run.OverflowTotals,
	}
	truthData, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	truthData = append(truthData, '\n')
	truthPath := filepath.Join(dir, "truth.json")
	if err := os.WriteFile(truthPath, truthData, 0o644); err != nil {
		return nil, model.NewError(model.EIO, truthPath, err.Error()).WithCause(err)
	}
	manifest.Files["truth"] = relativeFile(dir, truthPath, truthData)

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestData = append(manifestData, '\n')
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestData, 0o644); err != nil {
		return nil, model.NewError(model.EIO, "manifest.json", err.Error()).WithCause(err)
	}
	return manifest, nil
}

func relativeFile(root, path string, data []byte) string {
	sum := sha256.Sum256(data)
	relative, err := filepath.Rel(root, path)
	if err != nil {
		relative = path
	}
	return relative + "#" + hex.EncodeToString(sum[:])
}

// LoadTruth reads truth.json.
func LoadTruth(path string) (*Truth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.NewError(model.EIO, path, err.Error()).WithCause(err)
	}
	var truth Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, model.NewError(model.ESchema, path, err.Error()).WithCause(err)
	}
	return &truth, nil
}
