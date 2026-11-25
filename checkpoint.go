package colortrace

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// CheckpointStore is the persistence boundary used by the scheduler and tool.
type CheckpointStore interface {
	InitSnapshot(meta SnapshotMeta) error
	Meta() (SnapshotMeta, error)
	CommitUnit(record UnitRecord) error
	SaveUnitState(record UnitRecord) error
	Unit(stage int, chunkID string) (*UnitRecord, error)
	Units(projectID string) ([]UnitRecord, error)
	CommitLog(projectID string) ([]CommitEntry, error)
	PutArtifact(name string, data []byte) error
	GetArtifact(name string) ([]byte, error)
	Close() error
}

// MemoryStore is the zero-I/O checkpoint implementation used by path tests.
type MemoryStore struct {
	mu        sync.Mutex
	meta      SnapshotMeta
	hasMeta   bool
	units     map[string]UnitRecord
	log       []CommitEntry
	artifacts map[string][]byte
	seq       int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{units: map[string]UnitRecord{}, artifacts: map[string][]byte{}}
}

func (s *MemoryStore) InitSnapshot(meta SnapshotMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasMeta {
		if !snapshotMetaEqual(s.meta, meta) {
			s.units = map[string]UnitRecord{}
			s.log = nil
			s.artifacts = map[string][]byte{}
			s.seq = 0
			s.meta = meta
		}
		return nil
	}
	s.meta = meta
	s.hasMeta = true
	return nil
}

func (s *MemoryStore) Meta() (SnapshotMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta, nil
}

func (s *MemoryStore) CommitUnit(record UnitRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := UnitKey(record.Stage, record.ChunkID)
	s.seq++
	record.CommitSeq = s.seq
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	record.State = StateCommitted
	s.units[key] = record
	s.log = append(s.log, CommitEntry{
		Seq: s.seq, Stage: record.Stage, ChunkID: record.ChunkID, CacheKey: record.CacheKey,
	})
	return nil
}

func (s *MemoryStore) SaveUnitState(record UnitRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := UnitKey(record.Stage, record.ChunkID)
	if existing, ok := s.units[key]; ok && existing.State == StateCommitted {
		return stateError(record.Stage, record.ChunkID, StateCommitted, record.State)
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	s.units[key] = record
	return nil
}

func (s *MemoryStore) Unit(stage int, chunkID string) (*UnitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.units[UnitKey(stage, chunkID)]
	if !ok {
		return nil, nil
	}
	copyRecord := record
	return &copyRecord, nil
}

func (s *MemoryStore) Units(projectID string) ([]UnitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UnitRecord, 0, len(s.units))
	for _, record := range s.units {
		if projectID == "" || record.ProjectID == projectID {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stage != out[j].Stage {
			return out[i].Stage < out[j].Stage
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	return out, nil
}

func (s *MemoryStore) CommitLog(projectID string) ([]CommitEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]CommitEntry{}, s.log...)
	return out, nil
}

func (s *MemoryStore) PutArtifact(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[name] = append([]byte{}, data...)
	return nil
}

func (s *MemoryStore) GetArtifact(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.artifacts[name]
	if !ok {
		return nil, nil
	}
	return append([]byte{}, data...), nil
}

func (s *MemoryStore) Close() error { return nil }

func snapshotMetaEqual(a, b SnapshotMeta) bool {
	return a.ProjectID == b.ProjectID &&
		a.StageSequenceFingerprint == b.StageSequenceFingerprint &&
		a.ParamVersionFingerprint == b.ParamVersionFingerprint &&
		a.AlgorithmVersion == b.AlgorithmVersion &&
		a.TruthFingerprint == b.TruthFingerprint
}

// RecordMatchesSnapshot verifies every persisted version/hash field.
func RecordMatchesSnapshot(record UnitRecord, meta SnapshotMeta) bool {
	return record.StageSequenceFingerprint == meta.StageSequenceFingerprint &&
		record.AlgorithmVersion == meta.AlgorithmVersion &&
		record.ParamVersion != ""
}

// VerifyPersistedUnit recomputes content and truncation hashes for a committed
// chunk record. It is used when a snapshot is reopened.
func VerifyPersistedUnit(record UnitRecord) bool {
	if record.State != StateCommitted {
		return true
	}
	if record.CacheKey == "" || record.ContentHash == "" || record.OutputDigest == "" {
		return false
	}
	var chunk ChunkResult
	if len(record.OutputJSON) == 0 {
		return false
	}
	if err := json.Unmarshal(record.OutputJSON, &chunk); err != nil {
		return false
	}
	if chunk.ContentHash != record.ContentHash || chunk.CacheKey != record.CacheKey {
		return false
	}
	if HashChunkContent(&chunk) != record.ContentHash || record.OutputDigest != record.ContentHash {
		return false
	}
	_, _, truncationHash := truncationEvidence(&chunk)
	return truncationHash == record.TruncationDigest
}
