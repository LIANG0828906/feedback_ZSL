// Package checkpoint persists deterministic resolver state with database/sql
// and the vendored pure-Go modernc.org/sqlite driver.
package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"dnslab/codes"
	"dnslab/engine"

	_ "modernc.org/sqlite"
)

const DriverName = "sqlite"

// PendingFetch is a durable in-flight/back-source queue item.
type PendingFetch struct {
	ID       string `json:"id"`
	QuerySeq int    `json:"query_seq"`
	RegionID string `json:"region_id"`
	Payload  string `json:"payload_json"`
}

// CommittedQuery is one ordered query completion record.
type CommittedQuery struct {
	Sequence int           `json:"sequence"`
	QueryID  string        `json:"query_id"`
	Result   engine.Result `json:"result"`
}

// Checkpoint is the complete recoverable state at a commit point.
type Checkpoint struct {
	ProjectVersion string                    `json:"project_version"`
	SchemaVersion  string                    `json:"schema_version"`
	CommittedSeq   int                       `json:"committed_seq"`
	Segments       map[string]engine.Segment `json:"segments"`
	Pending        []PendingFetch            `json:"pending"`
	Queries        []CommittedQuery          `json:"queries"`
}

// Store owns a SQLite database handle.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates the database and schema when absent. Existing zero/short files
// are treated as truncated checkpoints rather than silently initialized.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, codes.New(codes.ECheckpointOpen, "checkpoint path is empty")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, codes.Wrap(codes.ECheckpointOpen, "cannot create checkpoint directory", err).
				WithContext("path", path)
		}
	}
	if st, err := os.Stat(path); err == nil && !st.IsDir() && st.Size() < 100 {
		return nil, codes.New(codes.ECheckpointTruncated, "checkpoint file is shorter than SQLite header size").
			WithContext("path", path).
			WithContext("size_bytes", st.Size()).
			WithContext("minimum_header_bytes", 100)
	}

	db, err := sql.Open(DriverName, path)
	if err != nil {
		return nil, codes.Wrap(codes.ECheckpointOpen, "cannot open sqlite checkpoint", err).
			WithContext("path", path)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, classifySQLiteError(err, path, "ping")
	}
	st := &Store{db: db, path: path}
	if err := st.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS cache_segments (
  region_id TEXT PRIMARY KEY,
  revision INTEGER NOT NULL,
  built_at_ms INTEGER NOT NULL,
  state_json BLOB NOT NULL,
  committed_seq INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pending_fetches (
  id TEXT PRIMARY KEY,
  query_seq INTEGER NOT NULL,
  region_id TEXT NOT NULL,
  payload_json BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS committed_queries (
  seq INTEGER PRIMARY KEY,
  query_id TEXT NOT NULL,
  result_json BLOB NOT NULL,
  committed_at_logical_ms INTEGER NOT NULL DEFAULT 0
);`)
	if err != nil {
		return classifySQLiteError(err, s.path, "create schema")
	}
	return nil
}

// Save writes one complete commit point. Cache segments are written before the
// global committed_seq meta key, so recovery never observes a new cursor over
// an unfinished segment transaction.
func (s *Store) Save(ctx context.Context, cp Checkpoint) error {
	if cp.ProjectVersion == "" {
		return codes.New(codes.ECheckpointOpen, "project version is required for checkpoint")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return codes.Wrap(codes.ECheckpointOpen, "cannot begin checkpoint transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Order 1: regional cache segment states.
	for regionID, seg := range cp.Segments {
		raw, err := json.Marshal(seg)
		if err != nil {
			return codes.Wrap(codes.ECheckpointCorrupt, "cannot encode cache segment", err).
				WithContext("region_id", regionID)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO cache_segments(region_id, revision, built_at_ms, state_json, committed_seq)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(region_id) DO UPDATE SET
  revision=excluded.revision,
  built_at_ms=excluded.built_at_ms,
  state_json=excluded.state_json,
  committed_seq=excluded.committed_seq`,
			regionID, seg.Revision, seg.BuiltAtMS, raw, cp.CommittedSeq); err != nil {
			return classifySQLiteError(err, s.path, "upsert cache segment")
		}
	}

	// Order 2: durable back-source queue.
	for _, item := range cp.Pending {
		raw, _ := json.Marshal(item)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO pending_fetches(id, query_seq, region_id, payload_json)
VALUES(?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  query_seq=excluded.query_seq,
  region_id=excluded.region_id,
  payload_json=excluded.payload_json`,
			item.ID, item.QuerySeq, item.RegionID, raw); err != nil {
			return classifySQLiteError(err, s.path, "upsert pending fetch")
		}
	}

	// Order 3: ordered query completion records.
	for _, q := range cp.Queries {
		raw, err := json.Marshal(q.Result)
		if err != nil {
			return codes.Wrap(codes.ECheckpointCorrupt, "cannot encode query result", err).
				WithContext("sequence", q.Sequence)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO committed_queries(seq, query_id, result_json)
VALUES(?, ?, ?)
ON CONFLICT(seq) DO UPDATE SET
  query_id=excluded.query_id,
  result_json=excluded.result_json`,
			q.Sequence, q.QueryID, raw); err != nil {
			return classifySQLiteError(err, s.path, "insert committed query")
		}
	}

	// Order 4 last: global cursor and version markers.
	meta := map[string]string{
		"committed_seq":     fmt.Sprint(cp.CommittedSeq),
		"project_version":   cp.ProjectVersion,
		"schema_version":    cp.SchemaVersion,
		"commit_order_note": "segments,pending,queries,then_global_seq",
	}
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO meta(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			return classifySQLiteError(err, s.path, "upsert meta")
		}
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(err, s.path, "commit")
	}
	return nil
}

// LoadRestored reads and validates a checkpoint. A nil checkpoint with nil
// error means no commit point exists.
func (s *Store) LoadRestored(ctx context.Context, expectedVersion string) (*Checkpoint, error) {
	var integrity string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return nil, classifySQLiteError(err, s.path, "integrity_check")
	}
	if integrity != "ok" {
		return nil, codes.New(codes.ECheckpointCorrupt, "sqlite integrity check failed").
			WithContext("path", s.path).WithContext("integrity", integrity)
	}

	var version string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='project_version'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, classifySQLiteError(err, s.path, "load project_version")
	}
	if expectedVersion != "" && version != expectedVersion {
		var schemaVersion, seqText string
		_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&schemaVersion)
		_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='committed_seq'`).Scan(&seqText)
		return nil, codes.New(codes.ECheckpointVersion, "checkpoint project version does not match current engineering snapshot").
			WithContext("path", s.path).
			WithContext("checkpoint_version", version).
			WithContext("expected_version", expectedVersion).
			WithContext("checkpoint_schema", schemaVersion).
			WithContext("committed_seq", seqText).
			WithContext("recovery_action", "restart_from_beginning")
	}

	cp := &Checkpoint{ProjectVersion: version, Segments: map[string]engine.Segment{}}
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&cp.SchemaVersion)
	var seqText string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='committed_seq'`).Scan(&seqText); err != nil {
		return nil, classifySQLiteError(err, s.path, "load committed_seq")
	}
	if _, err := fmt.Sscan(seqText, &cp.CommittedSeq); err != nil {
		return nil, codes.Wrap(codes.ECheckpointSequence, "committed_seq meta is invalid", err).
			WithContext("value", seqText)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT region_id, revision, built_at_ms, state_json FROM cache_segments`)
	if err != nil {
		return nil, classifySQLiteError(err, s.path, "query cache segments")
	}
	for rows.Next() {
		var regionID string
		var raw []byte
		var seg engine.Segment
		if err := rows.Scan(&regionID, &seg.Revision, &seg.BuiltAtMS, &raw); err != nil {
			return nil, classifySQLiteError(err, s.path, "scan cache segment")
		}
		if err := json.Unmarshal(raw, &seg); err != nil {
			return nil, codes.Wrap(codes.ECheckpointCorrupt, "cache segment JSON is incomplete", err).
				WithContext("region_id", regionID)
		}
		cp.Segments[regionID] = seg
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, classifySQLiteError(err, s.path, "iterate cache segments")
	}
	_ = rows.Close()

	prows, err := s.db.QueryContext(ctx, `SELECT id, query_seq, region_id, payload_json FROM pending_fetches ORDER BY query_seq, id`)
	if err != nil {
		return nil, classifySQLiteError(err, s.path, "query pending fetches")
	}
	for prows.Next() {
		var item PendingFetch
		var raw []byte
		if err := prows.Scan(&item.ID, &item.QuerySeq, &item.RegionID, &raw); err != nil {
			_ = prows.Close()
			return nil, classifySQLiteError(err, s.path, "scan pending fetch")
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			_ = prows.Close()
			return nil, codes.Wrap(codes.ECheckpointCorrupt, "pending fetch JSON is incomplete", err).
				WithContext("id", item.ID)
		}
		cp.Pending = append(cp.Pending, item)
	}
	if err := prows.Err(); err != nil {
		_ = prows.Close()
		return nil, classifySQLiteError(err, s.path, "iterate pending fetches")
	}
	_ = prows.Close()

	qrows, err := s.db.QueryContext(ctx, `SELECT seq, query_id, result_json FROM committed_queries ORDER BY seq`)
	if err != nil {
		return nil, classifySQLiteError(err, s.path, "query committed queries")
	}
	defer qrows.Close()
	maxSeq := 0
	for qrows.Next() {
		var q CommittedQuery
		var raw []byte
		if err := qrows.Scan(&q.Sequence, &q.QueryID, &raw); err != nil {
			return nil, classifySQLiteError(err, s.path, "scan committed query")
		}
		if err := json.Unmarshal(raw, &q.Result); err != nil {
			return nil, codes.Wrap(codes.ECheckpointCorrupt, "query result JSON is incomplete", err).
				WithContext("sequence", q.Sequence)
		}
		cp.Queries = append(cp.Queries, q)
		if q.Sequence > maxSeq {
			maxSeq = q.Sequence
		}
	}
	if maxSeq > cp.CommittedSeq {
		return nil, codes.New(codes.ECheckpointSequence, "committed query row is ahead of global sequence").
			WithContext("max_query_seq", maxSeq).
			WithContext("committed_seq", cp.CommittedSeq)
	}
	return cp, nil
}

// TruncateForInjection truncates an existing checkpoint to a short prefix.
func TruncateForInjection(path string, size int64) error {
	if size <= 0 {
		size = 32
	}
	if err := os.Truncate(path, size); err != nil {
		return codes.Wrap(codes.ECheckpointTruncated, "cannot truncate checkpoint for injection", err).
			WithContext("path", path)
	}
	return nil
}

func classifySQLiteError(err error, path, operation string) error {
	msg := err.Error()
	code := codes.ECheckpointCorrupt
	if containsAny(msg, "truncated", "EOF", "short read") {
		code = codes.ECheckpointTruncated
	}
	if containsAny(msg, "unable to open", "cannot open", "permission", "no such file or directory") {
		code = codes.ECheckpointOpen
	}
	return codes.Wrap(code, "sqlite checkpoint operation failed: "+operation, err).
		WithContext("path", path).
		WithContext("operation", operation)
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if len(p) > 0 && containsFold(s, p) {
			return true
		}
	}
	return false
}

func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			a, b := s[i+j], substr[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
