package repair

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"meshforge/internal/mesh"
)

const (
	ManifestFormat = "meshforge.checkpoint.manifest.v1"
	SnapshotFormat = "meshforge.checkpoint.snapshot.v1"
)

type CommitMeta struct {
	StageIndex int       `json:"stage_index"`
	Stage      StageName `json:"stage"`
	File       string    `json:"file"`
	MeshHash   string    `json:"mesh_hash"`
	ParentHash string    `json:"parent_hash"`
}

type Manifest struct {
	Format           string       `json:"format"`
	RunID            string       `json:"run_id"`
	InputHash        string       `json:"input_hash"`
	ParameterVersion string       `json:"parameter_version"`
	AlgorithmVersion string       `json:"algorithm_version"`
	Commits          []CommitMeta `json:"commits"`
}

type Snapshot struct {
	Format           string        `json:"format"`
	RunID            string        `json:"run_id"`
	InputHash        string        `json:"input_hash"`
	ParameterVersion string        `json:"parameter_version"`
	AlgorithmVersion string        `json:"algorithm_version"`
	StageIndex       int           `json:"stage_index"`
	Stage            StageName     `json:"stage"`
	ParentHash       string        `json:"parent_hash"`
	MeshHash         string        `json:"mesh_hash"`
	Mesh             mesh.Document `json:"mesh"`
	Stages           []StageRecord `json:"stages"`
}

type ResumeState struct {
	Manifest Manifest
	Snapshot Snapshot
	Mesh     mesh.Mesh
}

type CheckpointStore struct {
	Dir      string
	mu       sync.Mutex
	manifest Manifest
}

func NewCheckpointStore(dir string) *CheckpointStore {
	return &CheckpointStore{Dir: dir}
}

func (s *CheckpointStore) ManifestPath() string {
	return filepath.Join(s.Dir, "manifest.json")
}

func (s *CheckpointStore) Load(inputHash, paramVersion, algorithmVersion string) (*ResumeState, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifest = Manifest{}
	data, err := os.ReadFile(s.ManifestPath())
	if os.IsNotExist(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, "manifest_decode: " + err.Error(), nil
	}
	if manifest.Format != ManifestFormat {
		return nil, "manifest_format", nil
	}
	if manifest.InputHash != inputHash {
		return nil, "input_hash_mismatch", nil
	}
	if manifest.ParameterVersion != paramVersion {
		return nil, "parameter_version_mismatch", nil
	}
	if manifest.AlgorithmVersion != algorithmVersion {
		return nil, "algorithm_version_mismatch", nil
	}
	if len(manifest.Commits) == 0 || len(manifest.Commits) > len(StageOrder) {
		return nil, "commit_count_invalid", nil
	}
	sort.Slice(manifest.Commits, func(i, j int) bool {
		return manifest.Commits[i].StageIndex < manifest.Commits[j].StageIndex
	})
	var latest Snapshot
	expectedParent := inputHash
	seen := make(map[int]bool)
	for i, cm := range manifest.Commits {
		if cm.StageIndex != i || cm.Stage != StageOrder[i] || seen[cm.StageIndex] {
			return nil, "commit_sequence_invalid", nil
		}
		seen[cm.StageIndex] = true
		if cm.ParentHash != expectedParent {
			return nil, fmt.Sprintf("hash_chain_break_at_stage_%d", i), nil
		}
		path := filepath.Join(s.Dir, cm.File)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Sprintf("snapshot_read_stage_%d", i), nil
		}
		var snap Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			return nil, fmt.Sprintf("snapshot_decode_stage_%d", i), nil
		}
		if snap.Format != SnapshotFormat || snap.RunID != manifest.RunID ||
			snap.InputHash != inputHash || snap.ParameterVersion != paramVersion ||
			snap.AlgorithmVersion != algorithmVersion ||
			snap.StageIndex != i || snap.Stage != StageOrder[i] ||
			snap.ParentHash != expectedParent || snap.MeshHash != cm.MeshHash {
			return nil, fmt.Sprintf("snapshot_metadata_mismatch_stage_%d", i), nil
		}
		m := snap.Mesh.Mesh()
		actual := m.ContentHash()
		if actual != snap.MeshHash {
			return nil, fmt.Sprintf("content_hash_mismatch_stage_%d", i), nil
		}
		if len(snap.Stages) != len(StageOrder) {
			return nil, fmt.Sprintf("stage_matrix_invalid_stage_%d", i), nil
		}
		for j := 0; j <= i; j++ {
			if snap.Stages[j].Status != StatusCommitted {
				return nil, fmt.Sprintf("uncommitted_stage_in_snapshot_%d", j), nil
			}
		}
		expectedParent = cm.MeshHash
		latest = snap
	}
	s.manifest = manifest
	return &ResumeState{
		Manifest: manifest,
		Snapshot: latest,
		Mesh:     latest.Mesh.Mesh(),
	}, "", nil
}

func (s *CheckpointStore) Commit(snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	filename := fmt.Sprintf("stage-%02d-%s.json", snap.StageIndex, snap.Stage)
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if err := mesh.WriteFileAtomic(filepath.Join(s.Dir, filename), raw, 0o644); err != nil {
		return err
	}
	if s.manifest.Format == "" {
		s.manifest = Manifest{
			Format:           ManifestFormat,
			RunID:            snap.RunID,
			InputHash:        snap.InputHash,
			ParameterVersion: snap.ParameterVersion,
			AlgorithmVersion: snap.AlgorithmVersion,
		}
	}
	if s.manifest.RunID != snap.RunID ||
		s.manifest.InputHash != snap.InputHash ||
		s.manifest.ParameterVersion != snap.ParameterVersion ||
		s.manifest.AlgorithmVersion != snap.AlgorithmVersion {
		return fmt.Errorf("checkpoint manifest version changed during run")
	}
	for _, c := range s.manifest.Commits {
		if c.StageIndex == snap.StageIndex {
			return fmt.Errorf("stage %d already committed in checkpoint", snap.StageIndex)
		}
	}
	s.manifest.Commits = append(s.manifest.Commits, CommitMeta{
		StageIndex: snap.StageIndex,
		Stage:      snap.Stage,
		File:       filename,
		MeshHash:   snap.MeshHash,
		ParentHash: snap.ParentHash,
	})
	sort.Slice(s.manifest.Commits, func(i, j int) bool {
		return s.manifest.Commits[i].StageIndex < s.manifest.Commits[j].StageIndex
	})
	raw, err = json.MarshalIndent(s.manifest, "", "  ")
	if err != nil {
		s.manifest.Commits = s.manifest.Commits[:len(s.manifest.Commits)-1]
		return err
	}
	if err := mesh.WriteFileAtomic(s.ManifestPath(), append(raw, '\n'), 0o644); err != nil {
		s.manifest.Commits = s.manifest.Commits[:len(s.manifest.Commits)-1]
		_ = os.Remove(filepath.Join(s.Dir, filename))
		return err
	}
	return nil
}

func (s *CheckpointStore) Invalidate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifest = Manifest{}
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "manifest.json" ||
			(len(name) > 6 && name[:6] == "stage-") {
			if err := os.Remove(filepath.Join(s.Dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *CheckpointStore) ResetManifestForNewRun(runID, inputHash, paramVersion, algorithmVersion string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.manifest = Manifest{
		Format:           ManifestFormat,
		RunID:            runID,
		InputHash:        inputHash,
		ParameterVersion: paramVersion,
		AlgorithmVersion: algorithmVersion,
	}
}

func (s *CheckpointStore) LoadStageMesh(index int) (mesh.Mesh, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.manifest.Commits {
		if c.StageIndex != index {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.Dir, c.File))
		if err != nil {
			return mesh.Mesh{}, false
		}
		var snap Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			return mesh.Mesh{}, false
		}
		m := snap.Mesh.Mesh()
		if m.ContentHash() != c.MeshHash {
			return mesh.Mesh{}, false
		}
		return m, true
	}
	return mesh.Mesh{}, false
}

// SetParameterVersion records an uncommitted-stage parameter change. Old
// snapshots intentionally remain at their old version, so the next restart
// detects the mismatch and invalidates the entire chain.
func (s *CheckpointStore) SetParameterVersion(paramVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manifest.Format == "" {
		return nil
	}
	s.manifest.ParameterVersion = paramVersion
	raw, err := json.MarshalIndent(s.manifest, "", "  ")
	if err != nil {
		return err
	}
	return mesh.WriteFileAtomic(s.ManifestPath(), append(raw, '\n'), 0o644)
}
