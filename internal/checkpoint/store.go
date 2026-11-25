package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type FragmentCheckpoint struct {
	SnapshotID       string `json:"snapshot_id"`
	LogicalSeq       int64  `json:"logical_seq"`
	Epoch            int    `json:"epoch"`
	Ordinal          int    `json:"ordinal"`
	FragmentID       string `json:"fragment_id"`
	DialogID         string `json:"dialog_id"`
	EnvelopeDigest   string `json:"envelope_digest"`
	AccentSample     int64  `json:"accent_sample"`
	StretchJSON      string `json:"stretch_json"`
	ContentHash      string `json:"content_hash"`
	ParameterHash    string `json:"parameter_hash"`
	AlgorithmVersion string `json:"algorithm_version"`
	FrameCount       int    `json:"frame_count"`
	CommittedAt      string `json:"committed_at"`
}

type CacheEntry struct {
	Key              string `json:"key"`
	DialogID         string `json:"dialog_id"`
	PhonemeIndex     int    `json:"phoneme_index"`
	Kind             string `json:"kind"`
	PayloadJSON      string `json:"payload_json"`
	ContentHash      string `json:"content_hash"`
	ParameterHash    string `json:"parameter_hash"`
	AlgorithmVersion string `json:"algorithm_version"`
	UpdatedAt        string `json:"updated_at"`
}

type RecoveryBoundary struct {
	SnapshotID          string   `json:"snapshot_id"`
	SnapshotValid       bool     `json:"snapshot_valid"`
	LastLogicalSeq      int64    `json:"last_logical_seq"`
	LastEpoch           int      `json:"last_epoch"`
	LastOrdinal         int      `json:"last_ordinal"`
	ResumeLogicalSeq    int64    `json:"resume_logical_seq"`
	CompletedCommitKeys []string `json:"completed_commit_keys"`
	Mismatch            string   `json:"mismatch,omitempty"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS fragment_checkpoints (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  snapshot_id TEXT NOT NULL,
  logical_seq INTEGER NOT NULL,
  epoch INTEGER NOT NULL,
  ordinal INTEGER NOT NULL,
  fragment_id TEXT NOT NULL,
  dialog_id TEXT NOT NULL,
  envelope_digest TEXT NOT NULL,
  accent_sample INTEGER NOT NULL,
  stretch_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  parameter_hash TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  frame_count INTEGER NOT NULL,
  committed_at TEXT NOT NULL,
  UNIQUE(snapshot_id, epoch, fragment_id)
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_scan ON fragment_checkpoints(snapshot_id, logical_seq, id);
CREATE TABLE IF NOT EXISTS cache_entries (
  cache_key TEXT PRIMARY KEY,
  dialog_id TEXT NOT NULL,
  phoneme_index INTEGER NOT NULL,
  kind TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  parameter_hash TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  updated_at TEXT NOT NULL
);`)
	return err
}

func (s *Store) CommitFragment(c FragmentCheckpoint) error {
	return s.CommitFragmentWithEntries(c, nil)
}

func (s *Store) CommitFragmentWithEntries(c FragmentCheckpoint, entries []CacheEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.CommittedAt == "" {
		c.CommittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
INSERT INTO fragment_checkpoints
(snapshot_id,logical_seq,epoch,ordinal,fragment_id,dialog_id,envelope_digest,accent_sample,stretch_json,content_hash,parameter_hash,algorithm_version,frame_count,committed_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(snapshot_id,epoch,fragment_id) DO UPDATE SET
 logical_seq=excluded.logical_seq, ordinal=excluded.ordinal,
 envelope_digest=excluded.envelope_digest, accent_sample=excluded.accent_sample,
 stretch_json=excluded.stretch_json, content_hash=excluded.content_hash,
 parameter_hash=excluded.parameter_hash, algorithm_version=excluded.algorithm_version,
 frame_count=excluded.frame_count, committed_at=excluded.committed_at`,
		c.SnapshotID, c.LogicalSeq, c.Epoch, c.Ordinal, c.FragmentID, c.DialogID,
		c.EnvelopeDigest, c.AccentSample, c.StretchJSON, c.ContentHash, c.ParameterHash,
		c.AlgorithmVersion, c.FrameCount, c.CommittedAt)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.UpdatedAt == "" {
			e.UpdatedAt = c.CommittedAt
		}
		if _, err := tx.Exec(`INSERT INTO cache_entries
(cache_key,dialog_id,phoneme_index,kind,payload_json,content_hash,parameter_hash,algorithm_version,updated_at)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(cache_key) DO UPDATE SET payload_json=excluded.payload_json, updated_at=excluded.updated_at`,
			e.Key, e.DialogID, e.PhonemeIndex, e.Kind, e.PayloadJSON, e.ContentHash,
			e.ParameterHash, e.AlgorithmVersion, e.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) IsCommitted(snapshotID string, epoch int, fragmentID, contentHash, parameterHash, algoVersion string) (bool, error) {
	var gotContent, gotParam, gotAlgo string
	err := s.db.QueryRow(`SELECT content_hash,parameter_hash,algorithm_version FROM fragment_checkpoints WHERE snapshot_id=? AND epoch=? AND fragment_id=?`, snapshotID, epoch, fragmentID).Scan(&gotContent, &gotParam, &gotAlgo)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return gotContent == contentHash && gotParam == parameterHash && gotAlgo == algoVersion, nil
}

func (s *Store) RecoveryBoundary(snapshotID string, expectedContent map[string]string, expectedAlgo string) (RecoveryBoundary, error) {
	rows, err := s.db.Query(`SELECT logical_seq,epoch,ordinal,fragment_id,content_hash,algorithm_version FROM fragment_checkpoints WHERE snapshot_id=? ORDER BY logical_seq,id`, snapshotID)
	if err != nil {
		return RecoveryBoundary{}, err
	}
	defer rows.Close()
	b := RecoveryBoundary{SnapshotID: snapshotID, SnapshotValid: true, LastLogicalSeq: -1, ResumeLogicalSeq: 0}
	var expectedSeq int64
	for rows.Next() {
		var seq int64
		var epoch, ordinal int
		var fragmentID, contentHash, algo string
		if err := rows.Scan(&seq, &epoch, &ordinal, &fragmentID, &contentHash, &algo); err != nil {
			return b, err
		}
		if seq != expectedSeq {
			b.SnapshotValid = false
			b.Mismatch = fmt.Sprintf("non_contiguous_commit_sequence at logical_seq=%d expected=%d fragment=%s", seq, expectedSeq, fragmentID)
			break
		}
		if want := expectedContent[fragmentID]; want != "" && want != contentHash {
			b.SnapshotValid = false
			b.Mismatch = fmt.Sprintf("content hash mismatch at fragment=%s logical_seq=%d", fragmentID, seq)
			break
		}
		if expectedAlgo != "" && algo != expectedAlgo {
			b.SnapshotValid = false
			b.Mismatch = fmt.Sprintf("algorithm version mismatch at fragment=%s logical_seq=%d got=%s want=%s", fragmentID, seq, algo, expectedAlgo)
			break
		}
		b.LastLogicalSeq = seq
		b.LastEpoch = epoch
		b.LastOrdinal = ordinal
		b.CompletedCommitKeys = append(b.CompletedCommitKeys, fmt.Sprintf("%d/%s", epoch, fragmentID))
		expectedSeq++
	}
	if err := rows.Err(); err != nil {
		return b, err
	}
	b.ResumeLogicalSeq = b.LastLogicalSeq + 1
	return b, nil
}

func (s *Store) InvalidateSnapshot(snapshotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM fragment_checkpoints WHERE snapshot_id=?`, snapshotID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM cache_entries WHERE dialog_id IN (SELECT dialog_id FROM fragment_checkpoints WHERE snapshot_id=?)`, snapshotID); err != nil {
		// The rows have already been deleted in this tx; cache invalidation by snapshot key is handled by callers.
		_ = err
	}
	return tx.Commit()
}

func (s *Store) DeleteFromSequence(snapshotID string, logicalSeq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM fragment_checkpoints WHERE snapshot_id=? AND logical_seq>=?`, snapshotID, logicalSeq)
	return err
}

func (s *Store) PutCache(e CacheEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.UpdatedAt == "" {
		e.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.Exec(`INSERT INTO cache_entries
(cache_key,dialog_id,phoneme_index,kind,payload_json,content_hash,parameter_hash,algorithm_version,updated_at)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(cache_key) DO UPDATE SET payload_json=excluded.payload_json, updated_at=excluded.updated_at`,
		e.Key, e.DialogID, e.PhonemeIndex, e.Kind, e.PayloadJSON, e.ContentHash, e.ParameterHash, e.AlgorithmVersion, e.UpdatedAt)
	return err
}

func (s *Store) GetCache(key string) (CacheEntry, bool, error) {
	var e CacheEntry
	err := s.db.QueryRow(`SELECT cache_key,dialog_id,phoneme_index,kind,payload_json,content_hash,parameter_hash,algorithm_version,updated_at FROM cache_entries WHERE cache_key=?`, key).
		Scan(&e.Key, &e.DialogID, &e.PhonemeIndex, &e.Kind, &e.PayloadJSON, &e.ContentHash, &e.ParameterHash, &e.AlgorithmVersion, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CacheEntry{}, false, nil
	}
	if err != nil {
		return CacheEntry{}, false, err
	}
	return e, true, nil
}

func (s *Store) DeleteCache(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, key := range keys {
		if _, err := tx.Exec(`DELETE FROM cache_entries WHERE cache_key=?`, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) FindLatestCache(dialogID, kind string, phonemeIndex int) (CacheEntry, bool, error) {
	var e CacheEntry
	err := s.db.QueryRow(`SELECT cache_key,dialog_id,phoneme_index,kind,payload_json,content_hash,parameter_hash,algorithm_version,updated_at
FROM cache_entries WHERE dialog_id=? AND kind=? AND phoneme_index=?
ORDER BY updated_at DESC, rowid DESC LIMIT 1`, dialogID, kind, phonemeIndex).
		Scan(&e.Key, &e.DialogID, &e.PhonemeIndex, &e.Kind, &e.PayloadJSON, &e.ContentHash, &e.ParameterHash, &e.AlgorithmVersion, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CacheEntry{}, false, nil
	}
	if err != nil {
		return CacheEntry{}, false, err
	}
	return e, true, nil
}

func (s *Store) ListCheckpoints(snapshotID string) ([]FragmentCheckpoint, error) {
	rows, err := s.db.Query(`SELECT snapshot_id,logical_seq,epoch,ordinal,fragment_id,dialog_id,envelope_digest,accent_sample,stretch_json,content_hash,parameter_hash,algorithm_version,frame_count,committed_at FROM fragment_checkpoints WHERE snapshot_id=? ORDER BY logical_seq,id`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FragmentCheckpoint
	for rows.Next() {
		var c FragmentCheckpoint
		if err := rows.Scan(&c.SnapshotID, &c.LogicalSeq, &c.Epoch, &c.Ordinal, &c.FragmentID, &c.DialogID, &c.EnvelopeDigest, &c.AccentSample, &c.StretchJSON, &c.ContentHash, &c.ParameterHash, &c.AlgorithmVersion, &c.FrameCount, &c.CommittedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func MustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"marshal_failed"}`
	}
	return string(b)
}
