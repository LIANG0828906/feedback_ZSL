package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"audioeval/internal/model"
)

type CheckpointLoad struct {
	Resumed     bool
	Invalidated bool
	Reason      string
	Blocks      []model.BlockResult
}

type CheckpointStore struct {
	mu   sync.Mutex
	dir  string
	meta model.SnapshotMeta
}

func OpenCheckpoint(dir string, meta model.SnapshotMeta, resume bool) (*CheckpointStore, CheckpointLoad, error) {
	store := &CheckpointStore{dir: dir, meta: meta}
	load := CheckpointLoad{}
	if !resume {
		if err := resetDirectory(dir); err != nil {
			return nil, load, err
		}
		if err := store.writeMeta(); err != nil {
			return nil, load, err
		}
		return store, load, nil
	}

	metaPath := filepath.Join(dir, "meta.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := resetDirectory(dir); err != nil {
				return nil, load, err
			}
			if err := store.writeMeta(); err != nil {
				return nil, load, err
			}
			return store, load, nil
		}
		return nil, load, fmt.Errorf("read checkpoint metadata: %w", err)
	}
	var storedMeta model.SnapshotMeta
	if err := json.Unmarshal(metaData, &storedMeta); err != nil {
		load.Invalidated = true
		load.Reason = "checkpoint metadata is malformed"
		if err := resetDirectory(dir); err != nil {
			return nil, load, err
		}
		if err := store.writeMeta(); err != nil {
			return nil, load, err
		}
		return store, load, nil
	}
	if reason := metaMismatch(storedMeta, meta); reason != "" {
		load.Invalidated = true
		load.Reason = reason
		if err := resetDirectory(dir); err != nil {
			return nil, load, err
		}
		if err := store.writeMeta(); err != nil {
			return nil, load, err
		}
		return store, load, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, load, fmt.Errorf("scan checkpoint: %w", err)
	}
	results := make([]model.BlockResult, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "meta.json" || name == "report.json" || name == "partial-report.json" || name == "heatmap.png" {
			continue
		}
		sequence, ok := model.ParseBlockFileName(name)
		if !ok {
			if filepath.Ext(name) == ".tmp" || len(name) > 4 && name[:1] == "." {
				continue
			}
			load.Invalidated = true
			load.Reason = fmt.Sprintf("unexpected checkpoint file %s", name)
			break
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			load.Invalidated = true
			load.Reason = fmt.Sprintf("cannot read checkpoint block %d", sequence)
			break
		}
		var result model.BlockResult
		if err := json.Unmarshal(data, &result); err != nil {
			load.Invalidated = true
			load.Reason = fmt.Sprintf("checkpoint block %d is malformed", sequence)
			break
		}
		if result.Sequence != sequence {
			load.Invalidated = true
			load.Reason = fmt.Sprintf("checkpoint block file sequence %d contains sequence %d", sequence, result.Sequence)
			break
		}
		if result.InputHash != meta.InputHash || result.ParamVersion != meta.ParamVersion || result.AlgorithmVersion != meta.AlgorithmVersion {
			load.Invalidated = true
			load.Reason = fmt.Sprintf("checkpoint block %d has a version mismatch", sequence)
			break
		}
		if result.Frame < 0 || result.Frame >= meta.FrameCount || result.Block < 0 || result.Block >= meta.BlocksPerFrame {
			load.Invalidated = true
			load.Reason = fmt.Sprintf("checkpoint block %d has an invalid coordinate", sequence)
			break
		}
		if result.Sequence != result.Frame*meta.BlocksPerFrame+result.Block {
			load.Invalidated = true
			load.Reason = fmt.Sprintf("checkpoint block %d is not in canonical order", sequence)
			break
		}
		results = append(results, result)
	}
	if !load.Invalidated {
		sort.Slice(results, func(i, j int) bool { return results[i].Sequence < results[j].Sequence })
		for i, result := range results {
			if result.Sequence != i {
				load.Invalidated = true
				load.Reason = "checkpoint block sequence contains a gap"
				break
			}
		}
	}
	if load.Invalidated {
		if err := resetDirectory(dir); err != nil {
			return nil, load, err
		}
		if err := store.writeMeta(); err != nil {
			return nil, load, err
		}
		return store, load, nil
	}
	load.Resumed = len(results) > 0
	load.Blocks = results
	return store, load, nil
}

func (s *CheckpointStore) Commit(result model.BlockResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result.InputHash != s.meta.InputHash ||
		result.ParamVersion != s.meta.ParamVersion ||
		result.AlgorithmVersion != s.meta.AlgorithmVersion {
		return model.NewError(model.CodeState, "checkpoint_version", result.Frame, "block checkpoint version does not match the active snapshot").WithBlock(model.BlockCoord{Frame: result.Frame, Block: result.Block})
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checkpoint block: %w", err)
	}
	data = append(data, '\n')
	return s.atomicWrite(model.BlockFileName(result.Sequence), data)
}

func (s *CheckpointStore) Meta() model.SnapshotMeta {
	return s.meta
}

func (s *CheckpointStore) Dir() string {
	return s.dir
}

func (s *CheckpointStore) writeMeta() error {
	data, err := json.MarshalIndent(s.meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checkpoint metadata: %w", err)
	}
	data = append(data, '\n')
	return s.atomicWrite("meta.json", data)
}

func (s *CheckpointStore) atomicWrite(name string, data []byte) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create checkpoint directory: %w", err)
	}
	temp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary checkpoint: %w", err)
	}
	tempName := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temporary checkpoint: %w", err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary checkpoint: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("close temporary checkpoint: %w", err)
	}
	target := filepath.Join(s.dir, name)
	if existing, err := os.ReadFile(target); err == nil {
		_ = os.Remove(tempName)
		if bytes.Equal(existing, data) {
			return nil
		}
		return model.NewError(model.CodeState, "checkpoint", -1, "refusing to replace an existing checkpoint block with different contents")
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tempName)
		return fmt.Errorf("inspect existing checkpoint block: %w", err)
	}
	if err := os.Rename(tempName, target); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("publish checkpoint block: %w", err)
	}
	return nil
}

func resetDirectory(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("reset checkpoint directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create checkpoint directory: %w", err)
	}
	return nil
}

func metaMismatch(stored, active model.SnapshotMeta) string {
	switch {
	case stored.SchemaVersion != active.SchemaVersion:
		return "snapshot schema version mismatch"
	case stored.SampleID != active.SampleID:
		return "snapshot sample id mismatch"
	case stored.Quality != active.Quality:
		return "snapshot quality mismatch"
	case stored.InputHash != active.InputHash:
		return "snapshot input content hash mismatch"
	case stored.ParamVersion != active.ParamVersion:
		return "snapshot parameter version mismatch"
	case stored.AlgorithmVersion != active.AlgorithmVersion:
		return "snapshot algorithm version mismatch"
	case stored.TruthFingerprint != active.TruthFingerprint:
		return "snapshot truth fingerprint mismatch"
	case stored.FrameCount != active.FrameCount ||
		stored.FrameLength != active.FrameLength ||
		stored.FrameHop != active.FrameHop ||
		stored.Channels != active.Channels ||
		stored.BlocksPerFrame != active.BlocksPerFrame ||
		stored.Window != active.Window ||
		stored.WindowHop != active.WindowHop:
		return "snapshot frame geometry mismatch"
	default:
		return ""
	}
}
