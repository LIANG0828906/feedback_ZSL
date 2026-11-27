package terrainbake

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	CheckpointFormatVersion = 1
	AlgorithmVersion        = "terrainbake/adaptive-quadtree-v1"
	checkpointFileName      = "checkpoint.json"
)

type BlockState struct {
	HeightDeltaMM int64   `json:"height_delta_mm"`
	Budget        *Budget `json:"budget,omitempty"`
	ToleranceUM   *int64  `json:"tolerance_um,omitempty"`
	Fault         string  `json:"fault,omitempty"`
}

func (s BlockState) clone() BlockState {
	out := s
	if s.Budget != nil {
		b := *s.Budget
		out.Budget = &b
	}
	if s.ToleranceUM != nil {
		t := *s.ToleranceUM
		out.ToleranceUM = &t
	}
	return out
}

type BlockRecord struct {
	BlockID           string       `json:"block_id"`
	CacheKey          string       `json:"cache_key"`
	ContentHash       string       `json:"content_hash"`
	AdjacencyVersion  string       `json:"adjacency_version"`
	ParamVersion      string       `json:"param_version"`
	AlgorithmVersion  string       `json:"algorithm_version"`
	Dependencies      []string     `json:"dependencies"`
	UpstreamKeys      []string     `json:"upstream_keys"`
	VertexDigest      string       `json:"vertex_digest"`
	TriangleDigest    string       `json:"triangle_digest"`
	FoldDigest        string       `json:"fold_digest"`
	AttributionDigest string       `json:"attribution_digest"`
	RecordDigest      string       `json:"record_digest"`
	CommitSeq         int64        `json:"commit_seq"`
	Mesh              *Mesh        `json:"mesh"`
	Seams             []SeamReport `json:"seams"`
	Attribution       []string     `json:"attribution"`
}

type checkpointState struct {
	Format           int                    `json:"format"`
	AlgorithmVersion string                 `json:"algorithm_version"`
	Seed             uint64                 `json:"seed"`
	AdjacencyVersion string                 `json:"adjacency_version"`
	Edits            map[string]BlockState  `json:"edits"`
	NextSeq          int64                  `json:"next_seq"`
	Records          map[string]BlockRecord `json:"records"`
}

type CheckpointStore struct {
	mu   sync.Mutex
	dir  string
	path string
	data checkpointState
}

func OpenCheckpoint(dir string, p *Project) (*CheckpointStore, error) {
	store := &CheckpointStore{
		dir:  dir,
		path: filepath.Join(dir, checkpointFileName),
		data: checkpointState{
			Format:           CheckpointFormatVersion,
			AlgorithmVersion: AlgorithmVersion,
			Seed:             p.World.Seed,
			AdjacencyVersion: adjacencyVersion(p),
			Edits:            map[string]BlockState{},
			Records:          map[string]BlockRecord{},
		},
	}
	if dir == "" {
		return store, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, &Error{Code: CodeBlock, Field: "checkpoint_dir", Message: err.Error()}
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, &Error{Code: CodeDecode, Path: store.path, Field: "checkpoint", Message: err.Error()}
	}
	var loaded checkpointState
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, &Error{Code: CodeDecode, Path: store.path, Field: "checkpoint", Message: "corrupt checkpoint: " + err.Error()}
	}
	if loaded.Format != CheckpointFormatVersion {
		return nil, &Error{Code: CodeSchema, Path: "checkpoint.format", Field: "format", Message: fmt.Sprintf("unsupported checkpoint format %d", loaded.Format)}
	}
	if loaded.Seed != p.World.Seed {
		// The checkpoint belongs to another deterministic world. Keep its
		// bytes untouched but do not reuse any block.
		loaded.Records = map[string]BlockRecord{}
		loaded.Edits = map[string]BlockState{}
		loaded.NextSeq = 0
		loaded.AlgorithmVersion = AlgorithmVersion
		loaded.AdjacencyVersion = adjacencyVersion(p)
	}
	if loaded.Edits == nil {
		loaded.Edits = map[string]BlockState{}
	}
	if loaded.Records == nil {
		loaded.Records = map[string]BlockRecord{}
	}
	if loaded.AlgorithmVersion == "" {
		loaded.AlgorithmVersion = AlgorithmVersion
	}
	store.data = loaded
	return store, nil
}

func (s *CheckpointStore) StateSnapshot() (map[string]BlockState, int64) {
	if s == nil {
		return map[string]BlockState{}, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]BlockState, len(s.data.Edits))
	for k, v := range s.data.Edits {
		out[k] = v.clone()
	}
	return out, s.data.NextSeq
}

func (s *CheckpointStore) SaveEdits(edits map[string]BlockState) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Edits = make(map[string]BlockState, len(edits))
	for k, v := range edits {
		state := v.clone()
		state.Fault = ""
		s.data.Edits[k] = state
	}
	return s.saveLocked()
}

func (s *CheckpointStore) CommitRecord(rec BlockRecord) (BlockRecord, error) {
	if s == nil {
		rec.CommitSeq = 1
		rec.RecordDigest = recordDigest(rec)
		return rec, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.NextSeq++
	rec.CommitSeq = s.data.NextSeq
	rec.AlgorithmVersion = AlgorithmVersion
	rec.RecordDigest = recordDigest(rec)
	s.data.Records[rec.BlockID] = rec
	if err := s.saveLocked(); err != nil {
		return BlockRecord{}, err
	}
	return rec, nil
}

func (s *CheckpointStore) InvalidateBlock(blockID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Records[blockID]; !ok {
		return nil
	}
	delete(s.data.Records, blockID)
	return s.saveLocked()
}

func (s *CheckpointStore) Record(blockID string) (BlockRecord, bool) {
	if s == nil {
		return BlockRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.data.Records[blockID]
	return rec, ok
}

func (s *CheckpointStore) ValidateRecord(rec BlockRecord, expectedKey, content, adj, param string) bool {
	if rec.BlockID == "" || rec.Mesh == nil {
		return false
	}
	if rec.AlgorithmVersion != AlgorithmVersion ||
		rec.ContentHash != content ||
		rec.AdjacencyVersion != adj ||
		rec.ParamVersion != param ||
		rec.CacheKey != expectedKey {
		return false
	}
	if rec.VertexDigest != digestVertices(rec.Mesh.Vertices) ||
		rec.TriangleDigest != digestTriangles(rec.Mesh.Triangles) {
		return false
	}
	if rec.RecordDigest != recordDigest(rec) {
		return false
	}
	return true
}

func (s *CheckpointStore) saveLocked() error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".checkpoint-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func recordDigest(rec BlockRecord) string {
	rec.RecordDigest = ""
	return digestJSON(rec)
}

func attributionLabels(seams []SeamReport) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(seams))
	for _, s := range seams {
		if s.Attribution == "" || seen[s.Attribution] {
			continue
		}
		seen[s.Attribution] = true
		out = append(out, s.Attribution)
	}
	sort.Strings(out)
	return out
}
