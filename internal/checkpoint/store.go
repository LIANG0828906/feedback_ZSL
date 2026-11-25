package checkpoint

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"presentkernel/internal/domain"
)

type Store struct {
	db   *sql.DB
	path string
}

type RunRow struct {
	RunID            string `json:"run_id"`
	SequenceNo       int    `json:"sequence_no"`
	PartitionID      string `json:"partition_id"`
	DialogueID       string `json:"dialogue_id"`
	PartitionIndex   int    `json:"partition_index"`
	CacheKey         string `json:"cache_key"`
	ContentHash      string `json:"content_hash"`
	FrameDigest      string `json:"frame_digest"`
	SourceSchema     string `json:"source_schema"`
	QuantVersion     string `json:"quant_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	CommittedAt      string `json:"committed_at"`
}

type Status struct {
	Path          string   `json:"path"`
	RunID         string   `json:"run_id"`
	RunRows       int      `json:"run_rows"`
	CacheEntries  int      `json:"cache_entries"`
	LastSequence  int      `json:"last_sequence"`
	Rows          []RunRow `json:"rows"`
	SnapshotValid bool     `json:"snapshot_valid"`
	InvalidReason string   `json:"invalid_reason,omitempty"`
}

func Open(path string) (*Store, error) {
	if path != ":memory:" && path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS partition_cache (
	cache_key TEXT PRIMARY KEY,
	partition_id TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	source_schema TEXT NOT NULL,
	quant_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	result_json TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS run_checkpoint (
	run_id TEXT NOT NULL,
	sequence_no INTEGER NOT NULL,
	partition_id TEXT NOT NULL,
	dialogue_id TEXT NOT NULL,
	partition_index INTEGER NOT NULL,
	cache_key TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	frame_digest TEXT NOT NULL,
	frame_digests_json TEXT NOT NULL,
	source_schema TEXT NOT NULL,
	quant_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	result_json TEXT NOT NULL,
	committed_at TEXT NOT NULL DEFAULT (datetime('now')),
	PRIMARY KEY (run_id, sequence_no),
	UNIQUE (run_id, partition_id)
);
CREATE INDEX IF NOT EXISTS idx_run_checkpoint_order ON run_checkpoint(run_id, sequence_no);
`)
	return err
}

// CommitPartition atomically writes the content-addressed result and the
// ordered run checkpoint. MaxOpenConns(1) plus one transaction prevents
// interleaved partition commits.
func (s *Store) CommitPartition(runID string, sequence int, r *domain.PartitionResult) error {
	digests, err := json.Marshal(r.FrameDigests)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO partition_cache
		(cache_key, partition_id, content_hash, source_schema, quant_version, algorithm_version, result_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET result_json=excluded.result_json`,
		r.CacheKey, r.PartitionID, r.ContentHash, r.SourceSchema, r.QuantParamVersion, r.AlgorithmVersion, string(raw)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO run_checkpoint
		(run_id, sequence_no, partition_id, dialogue_id, partition_index, cache_key, content_hash,
		 frame_digest, frame_digests_json, source_schema, quant_version, algorithm_version, result_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, sequence_no) DO UPDATE SET
			partition_id=excluded.partition_id, cache_key=excluded.cache_key,
			content_hash=excluded.content_hash, frame_digest=excluded.frame_digest,
			frame_digests_json=excluded.frame_digests_json, result_json=excluded.result_json,
			committed_at=datetime('now')`,
		runID, sequence, r.PartitionID, r.DialogueID, r.PartitionIndex, r.CacheKey, r.ContentHash,
		r.Digest, string(digests), r.SourceSchema, r.QuantParamVersion, r.AlgorithmVersion, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetCached(cacheKey string) (*domain.PartitionResult, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT result_json FROM partition_cache WHERE cache_key=?`, cacheKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var r domain.PartitionResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, false, err
	}
	return &r, true, nil
}

func (s *Store) LoadRun(runID string) ([]*domain.PartitionResult, []RunRow, error) {
	rows, err := s.db.Query(`SELECT run_id, sequence_no, partition_id, dialogue_id, partition_index,
		cache_key, content_hash, frame_digest, source_schema, quant_version, algorithm_version, committed_at, result_json
		FROM run_checkpoint WHERE run_id=? ORDER BY sequence_no ASC`, runID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	results := []*domain.PartitionResult{}
	meta := []RunRow{}
	for rows.Next() {
		var row RunRow
		var raw string
		if err := rows.Scan(&row.RunID, &row.SequenceNo, &row.PartitionID, &row.DialogueID, &row.PartitionIndex,
			&row.CacheKey, &row.ContentHash, &row.FrameDigest, &row.SourceSchema, &row.QuantVersion,
			&row.AlgorithmVersion, &row.CommittedAt, &raw); err != nil {
			return nil, nil, err
		}
		var r domain.PartitionResult
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, nil, err
		}
		r.CommitSequence = row.SequenceNo
		results = append(results, &r)
		meta = append(meta, row)
	}
	return results, meta, rows.Err()
}

func (s *Store) InvalidateRun(runID string) error {
	_, err := s.db.Exec(`DELETE FROM run_checkpoint WHERE run_id=?`, runID)
	return err
}

func (s *Store) ResetAllRuns() error {
	_, err := s.db.Exec(`DELETE FROM run_checkpoint`)
	return err
}

// ValidateRun checks every committed row against the cache keys produced by
// the current project/parameters. The expected map is partition ID -> key.
// On any mismatch the ordered resume snapshot is deleted as a whole; the
// content-addressed partition_cache remains available for unaffected keys.
func (s *Store) ValidateRun(runID string, expectedKeys map[string]string) (bool, string, []RunRow, error) {
	_, rows, err := s.LoadRun(runID)
	if err != nil {
		return false, "", nil, err
	}
	seen := map[string]bool{}
	for i, row := range rows {
		if row.SequenceNo != i+1 {
			_ = s.InvalidateRun(runID)
			return false, fmt.Sprintf("non-contiguous sequence at %s: got %d want %d", row.PartitionID, row.SequenceNo, i+1), rows, nil
		}
		want, ok := expectedKeys[row.PartitionID]
		if !ok {
			_ = s.InvalidateRun(runID)
			return false, "checkpoint contains unknown partition " + row.PartitionID, rows, nil
		}
		if want != row.CacheKey || row.QuantVersion != quantVersion() || row.AlgorithmVersion != domain.AlgorithmVersion {
			_ = s.InvalidateRun(runID)
			return false, "content hash or quantization/algorithm version mismatch at " + row.PartitionID, rows, nil
		}
		seen[row.PartitionID] = true
	}
	return true, "", rows, nil
}

func quantVersion() string { return "quant-int-v1" }

func (s *Store) Status(runID string) (*Status, error) {
	st := &Status{Path: s.path, RunID: runID}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM run_checkpoint WHERE run_id=?`, runID).Scan(&st.RunRows); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM partition_cache`).Scan(&st.CacheEntries); err != nil {
		return nil, err
	}
	_, rows, err := s.LoadRun(runID)
	if err != nil {
		return nil, err
	}
	st.Rows = rows
	if len(rows) > 0 {
		st.LastSequence = rows[len(rows)-1].SequenceNo
	}
	st.SnapshotValid = true
	return st, nil
}
