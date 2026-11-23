// Package checkpoint persists committed processing chunks so an
// interrupted run can resume. Every commit is written atomically
// (temp file + rename) and serialised through a single mutex, so
// records never interleave. A snapshot is valid only when the project
// content hash, the time-unit parameter version and the algorithm
// version all match; any mismatch discards the whole snapshot - partial
// mixing is forbidden.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"captionx/internal/errs"
	"captionx/internal/model"
)

// Version tags stamped into every snapshot.
const (
	TimeUnitVersion = "tu-v1"   // integer-sample time unit parameters
	AlgoVersion     = "algo-v1" // derivation / scheduling algorithm
	snapshotFile    = "snapshot.json"
)

// ChunkRecord is the persisted result of one committed chunk.
type ChunkRecord struct {
	ChunkIdx   int                       `json:"chunkIdx"`
	Captions   []model.CaptionResult     `json:"captions"`
	Decisions  []model.Decision          `json:"decisions"`
	ErrorTerms []model.CaptionErrorTerms `json:"errorTerms"`
}

// Snapshot is the top-level checkpoint manifest.
type Snapshot struct {
	ContentHash     string `json:"contentHash"`
	TimeUnitVersion string `json:"timeUnitVersion"`
	AlgoVersion     string `json:"algoVersion"`
	Committed       []int  `json:"committed"` // chunk indices, ascending
}

// Store is a directory-backed checkpoint store.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore opens (or creates) the store rooted at dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errs.IO(fmt.Sprintf("create checkpoint dir: %v", err))
	}
	return &Store{dir: dir}, nil
}

func (s *Store) chunkPath(idx int) string {
	return filepath.Join(s.dir, fmt.Sprintf("chunk-%04d.json", idx))
}

// writeAtomic serialises v to a temp file and renames it into place.
func writeAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errs.IO(fmt.Sprintf("marshal %s: %v", path, err))
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return errs.IO(fmt.Sprintf("write %s: %v", tmp, err))
	}
	if err := os.Rename(tmp, path); err != nil {
		return errs.IO(fmt.Sprintf("rename %s: %v", path, err))
	}
	return nil
}

// Commit atomically and serially persists one committed chunk plus the
// updated snapshot manifest.
func (s *Store) Commit(rec ChunkRecord, snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap.TimeUnitVersion = TimeUnitVersion
	snap.AlgoVersion = AlgoVersion
	// keep the committed list duplicate-free, ascending
	var committed []int
	for _, c := range snap.Committed {
		if c != rec.ChunkIdx {
			committed = append(committed, c)
		}
	}
	committed = append(committed, rec.ChunkIdx)
	for i := 1; i < len(committed); i++ {
		for j := i; j > 0 && committed[j-1] > committed[j]; j-- {
			committed[j-1], committed[j] = committed[j], committed[j-1]
		}
	}
	snap.Committed = committed
	if err := writeAtomic(s.chunkPath(rec.ChunkIdx), rec); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, snapshotFile), snap)
}

// Load reads the snapshot and all committed chunk records. If the
// content hash or any version mismatches, the whole snapshot is
// invalidated (files removed) and (nil, nil) is returned so the caller
// recomputes from scratch.
func (s *Store) Load(contentHash string) (*Snapshot, []ChunkRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(s.dir, snapshotFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, errs.IO(fmt.Sprintf("read snapshot: %v", err))
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		s.discardLocked() // corrupt snapshot: discard entirely
		return nil, nil, nil
	}
	if snap.ContentHash != contentHash ||
		snap.TimeUnitVersion != TimeUnitVersion ||
		snap.AlgoVersion != AlgoVersion {
		s.discardLocked()
		return nil, nil, nil
	}
	var recs []ChunkRecord
	for _, idx := range snap.Committed {
		rb, err := os.ReadFile(s.chunkPath(idx))
		if err != nil {
			s.discardLocked() // missing record: snapshot unusable as a whole
			return nil, nil, nil
		}
		var rec ChunkRecord
		if err := json.Unmarshal(rb, &rec); err != nil {
			s.discardLocked()
			return nil, nil, nil
		}
		recs = append(recs, rec)
	}
	return &snap, recs, nil
}

// discardLocked removes every persisted file. Caller holds the mutex.
func (s *Store) discardLocked() {
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		_ = os.Remove(filepath.Join(s.dir, e.Name()))
	}
}

// Discard removes all persisted state.
func (s *Store) Discard() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discardLocked()
}
