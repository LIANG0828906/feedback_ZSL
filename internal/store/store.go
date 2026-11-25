// Package store persists fixed-segment settlement checkpoints in SQLite.
// Writes are atomic transactions and serialized by an in-process mutex so
// checkpoint payloads can never interleave.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"replayaudit/internal/model"
)

// Meta is the snapshot validity envelope.
type Meta struct {
	ReplayID      string `json:"replay_id"`
	ContentHash   string `json:"content_hash"`
	FormatVersion string `json:"format_version"`
	ParamsVersion string `json:"params_version"`
	AlgoVersion   string `json:"algo_version"`
	SegmentCount  int    `json:"segment_count"`
	LastCommitSeq int    `json:"last_commit_seq"`
}

// SegmentRecord is the persisted payload of one committed segment.
type SegmentRecord struct {
	model.SegmentCommit
	// State is the cumulative settlement state at the segment boundary.
	State   model.State          `json:"state"`
	Results []*model.EventResult `json:"results"`
}

// Store is the serialized SQLite checkpoint handle.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// OpenFile opens (creating if needed) a SQLite database at path.
func OpenFile(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed // Windows drive paths -> /C:/... for file URI
	}
	dsn := fmt.Sprintf("file://%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", slashed)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // connection-level serialization as a second guard
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// OpenMemory opens an isolated in-memory database (mainly for tests).
func OpenMemory() (*Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS checkpoint_meta (
  replay_id TEXT PRIMARY KEY,
  content_hash TEXT NOT NULL,
  format_version TEXT NOT NULL,
  params_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  segment_count INTEGER NOT NULL,
  last_commit_seq INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS segment_checkpoints (
  replay_id TEXT NOT NULL,
  segment_index INTEGER NOT NULL,
  commit_seq INTEGER NOT NULL,
  payload TEXT NOT NULL,
  PRIMARY KEY (replay_id, segment_index)
);`)
	return err
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// LoadMeta reads the validity envelope; nil when no snapshot exists.
func (s *Store) LoadMeta(replayID string) (*Meta, error) {
	row := s.db.QueryRow(`SELECT replay_id,content_hash,format_version,params_version,algo_version,segment_count,last_commit_seq
		FROM checkpoint_meta WHERE replay_id=?`, replayID)
	var m Meta
	err := row.Scan(&m.ReplayID, &m.ContentHash, &m.FormatVersion, &m.ParamsVersion,
		&m.AlgoVersion, &m.SegmentCount, &m.LastCommitSeq)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ResetReplay atomically drops every checkpoint of one replay.
func (s *Store) ResetReplay(replayID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM segment_checkpoints WHERE replay_id=?`, replayID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM checkpoint_meta WHERE replay_id=?`, replayID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// CheckCompatible returns true when the stored envelope matches content hash
// and all three versions. On any mismatch the whole replay snapshot is purged
// so old segment results can never be mixed with a new run.
func (s *Store) CheckCompatible(replayID, contentHash, formatVersion, paramsVersion, algoVersion string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadMetaLocked(replayID)
	if err != nil {
		return false, err
	}
	if m == nil {
		return false, nil
	}
	if m.ContentHash == contentHash && m.FormatVersion == formatVersion &&
		m.ParamsVersion == paramsVersion && m.AlgoVersion == algoVersion {
		return true, nil
	}
	if _, err := s.db.Exec(`DELETE FROM segment_checkpoints WHERE replay_id=?`, replayID); err != nil {
		return false, err
	}
	if _, err := s.db.Exec(`DELETE FROM checkpoint_meta WHERE replay_id=?`, replayID); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) loadMetaLocked(replayID string) (*Meta, error) {
	row := s.db.QueryRow(`SELECT replay_id,content_hash,format_version,params_version,algo_version,segment_count,last_commit_seq
		FROM checkpoint_meta WHERE replay_id=?`, replayID)
	var m Meta
	err := row.Scan(&m.ReplayID, &m.ContentHash, &m.FormatVersion, &m.ParamsVersion,
		&m.AlgoVersion, &m.SegmentCount, &m.LastCommitSeq)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// CommitSegment atomically upserts one segment payload and the meta envelope.
// The mutex makes writes serial; callers still commit segments in index order.
func (s *Store) CommitSegment(m Meta, rec SegmentRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO segment_checkpoints
		(replay_id,segment_index,commit_seq,payload) VALUES (?,?,?,?)`,
		m.ReplayID, rec.Segment, rec.CommitSeq, string(payload)); err != nil {
		_ = tx.Rollback()
		return err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM segment_checkpoints WHERE replay_id=?`, m.ReplayID).Scan(&count); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO checkpoint_meta
		(replay_id,content_hash,format_version,params_version,algo_version,segment_count,last_commit_seq)
		VALUES (?,?,?,?,?,?,?)`,
		m.ReplayID, m.ContentHash, m.FormatVersion, m.ParamsVersion, m.AlgoVersion, count, rec.CommitSeq); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// LoadSegments returns all persisted segment records ordered by segment index.
func (s *Store) LoadSegments(replayID string) ([]SegmentRecord, *Meta, error) {
	rows, err := s.db.Query(`SELECT payload FROM segment_checkpoints WHERE replay_id=? ORDER BY segment_index ASC`, replayID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []SegmentRecord
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, nil, err
		}
		var rec SegmentRecord
		if err := json.Unmarshal([]byte(payload), &rec); err != nil {
			return nil, nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	m, err := s.LoadMeta(replayID)
	if err != nil {
		return nil, nil, err
	}
	return out, m, nil
}
