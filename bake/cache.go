package bake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"terrain-bake/internal/atomicfs"
	"terrain-bake/project"
)

const snapshotSchemaVersion = 1

type SnapshotEntry struct {
	BlockID        string   `json:"blockId"`
	CacheKey       string   `json:"cacheKey"`
	Dependencies   []string `json:"dependencies"`
	ArtifactType   string   `json:"artifactType"`
	ArtifactSHA256 string   `json:"artifactSha256"`
	Artifact       []byte   `json:"artifact"`
}

type Snapshot struct {
	SchemaVersion        int             `json:"schemaVersion"`
	ContentHash          string          `json:"contentHash"`
	BakeAlgorithmVersion string          `json:"bakeAlgorithmVersion"`
	BakeParamsVersion    string          `json:"bakeParamsVersion"`
	BakeParamsHash       string          `json:"bakeParamsHash"`
	Generation           uint64          `json:"generation"`
	CommitOrder          []string        `json:"commitOrder"`
	Entries              []SnapshotEntry `json:"entries"`
}

type SnapshotWriteResult struct {
	Path       string `json:"path"`
	Hash       string `json:"hash"`
	EntryCount int    `json:"entryCount"`
	Generation uint64 `json:"generation"`
}

type LoadedCache struct {
	Valid        bool                 `json:"valid"`
	Present      bool                 `json:"present"`
	Reason       string               `json:"reason"`
	Generation   uint64               `json:"generation"`
	SnapshotHash string               `json:"snapshotHash"`
	Artifacts    map[string]*Artifact `json:"-"`
}

type SnapshotStore struct {
	Dir  string
	Path string
	mu   sync.Mutex
}

func NewSnapshotStore(dir string) *SnapshotStore {
	return &SnapshotStore{
		Dir:  dir,
		Path: filepath.Join(dir, "snapshot.json"),
	}
}

func CacheKey(contentHash, paramsVersion, paramsHash, blockID string, dependencies []string) string {
	deps := append([]string(nil), dependencies...)
	sort.Strings(deps)
	payload, _ := json.Marshal(struct {
		ContentHash   string   `json:"contentHash"`
		ParamsVersion string   `json:"paramsVersion"`
		ParamsHash    string   `json:"paramsHash"`
		BlockID       string   `json:"blockId"`
		Dependencies  []string `json:"dependencies"`
	}{contentHash, paramsVersion, paramsHash, blockID, deps})
	return project.HashBytes(payload)
}

func (s *SnapshotStore) Load(p *project.Project, g *Graph, params Params) LoadedCache {
	result := LoadedCache{Artifacts: make(map[string]*Artifact)}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			result.Reason = "snapshot not found"
		} else {
			result.Reason = "read snapshot: " + err.Error()
		}
		return result
	}
	result.Present = true
	result.SnapshotHash = project.HashBytes(data)
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		result.Reason = "decode snapshot: " + err.Error()
		return result
	}
	if snap.SchemaVersion != snapshotSchemaVersion {
		result.Reason = "unsupported snapshot schema version"
		return result
	}
	if snap.ContentHash != p.ContentHash {
		result.Reason = "global content hash mismatch"
		return result
	}
	if snap.BakeAlgorithmVersion != AlgorithmVersion ||
		snap.BakeParamsVersion != params.ParamsVersion ||
		snap.BakeParamsHash != params.ArtifactHash() {
		result.Reason = "bake parameter version/hash mismatch"
		return result
	}
	seen := map[string]bool{}
	for _, entry := range snap.Entries {
		if seen[entry.BlockID] {
			result.Reason = "duplicate block entry " + entry.BlockID
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		seen[entry.BlockID] = true
		if _, ok := p.Blocks[entry.BlockID]; !ok {
			result.Reason = "snapshot references unknown block " + entry.BlockID
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		deps := g.BlockDependencies(entry.BlockID)
		if !equalStrings(entry.Dependencies, deps) {
			result.Reason = "dependency set mismatch for " + entry.BlockID
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		expectedKey := CacheKey(p.ContentHash, params.ParamsVersion, params.ArtifactHash(), entry.BlockID, deps)
		if entry.CacheKey != expectedKey {
			result.Reason = "cache key mismatch for " + entry.BlockID
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		if entry.ArtifactType != "terrain-batch-v1" {
			result.Reason = "unsupported artifact type for " + entry.BlockID
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		if project.HashBytes(entry.Artifact) != entry.ArtifactSHA256 {
			result.Reason = "artifact checksum mismatch for " + entry.BlockID
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		var artifact Artifact
		if err := json.Unmarshal(entry.Artifact, &artifact); err != nil {
			result.Reason = "decode artifact for " + entry.BlockID + ": " + err.Error()
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		if artifact.BlockID != entry.BlockID {
			result.Reason = "artifact block id mismatch for " + entry.BlockID
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		if e := validateArtifactShape(p, params, &artifact); e != nil {
			result.Reason = "artifact shape mismatch for " + entry.BlockID + ": " + e.Message
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		if artifact.Hash() != entry.ArtifactSHA256 {
			result.Reason = "non-canonical artifact for " + entry.BlockID
			result.Artifacts = map[string]*Artifact{}
			return result
		}
		result.Artifacts[entry.BlockID] = &artifact
	}
	result.Valid = true
	result.Generation = snap.Generation
	result.Reason = "snapshot loaded"
	return result
}

func (s *SnapshotStore) Write(p *project.Project, g *Graph, params Params, generation uint64, order []string, artifacts map[string]*Artifact) (SnapshotWriteResult, *project.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var snap Snapshot
	snap.SchemaVersion = snapshotSchemaVersion
	snap.ContentHash = p.ContentHash
	snap.BakeAlgorithmVersion = AlgorithmVersion
	snap.BakeParamsVersion = params.ParamsVersion
	snap.BakeParamsHash = params.ArtifactHash()
	snap.Generation = generation
	for _, id := range order {
		a := artifacts[id]
		if a == nil {
			continue
		}
		data := a.CanonicalBytes()
		deps := g.BlockDependencies(id)
		snap.CommitOrder = append(snap.CommitOrder, id)
		snap.Entries = append(snap.Entries, SnapshotEntry{
			BlockID:        id,
			CacheKey:       CacheKey(p.ContentHash, params.ParamsVersion, params.ArtifactHash(), id, deps),
			Dependencies:   deps,
			ArtifactType:   "terrain-batch-v1",
			ArtifactSHA256: project.HashBytes(data),
			Artifact:       data,
		})
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return SnapshotWriteResult{}, project.NewError(project.CodeCache, "", "snapshot", "encode snapshot: %v", err)
	}
	data = append(data, '\n')
	if err := atomicfs.WriteFile(s.Path, data, 0o644); err != nil {
		return SnapshotWriteResult{}, project.NewError(project.CodeCache, "", "snapshot", "write snapshot atomically: %v", err)
	}
	return SnapshotWriteResult{
		Path:       s.Path,
		Hash:       project.HashBytes(data),
		EntryCount: len(snap.Entries),
		Generation: generation,
	}, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
