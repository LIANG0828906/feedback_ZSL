package checkpoint

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"skeletonretarget/internal/model"
)

const SchemaVersion = "1"

type Meta struct {
	ActionHash       string
	AlgorithmVersion string
}

type IntervalRecord struct {
	IntervalID       int             `json:"intervalId"`
	FrameStart       int             `json:"frameStart"`
	FrameEnd         int             `json:"frameEnd"`
	State            string          `json:"state"`
	Payload          []byte          `json:"payload"`
	ContentHash      string          `json:"contentHash"`
	Summary          json.RawMessage `json:"summary"`
	Dependencies     []string        `json:"dependencies"`
	DependencyHash   string          `json:"dependencyHash"`
	CacheKey         string          `json:"cacheKey"`
	TopologyVersion  string          `json:"topologyVersion"`
	ParamVersion     string          `json:"paramVersion"`
	AlgorithmVersion string          `json:"algorithmVersion"`
	StateProgress    int             `json:"stateProgress"`
}

type ResumeResult struct {
	Records         []IntervalRecord
	InvalidatedFrom int
	Invalidated     bool
	Reason          string
}

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
	path    string
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, &model.Error{Code: model.CodeState, Frame: -1, Message: "checkpoint path is empty"}
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS schema_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`INSERT INTO schema_meta(key, value) VALUES('schema_version', '1')
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		`CREATE TABLE IF NOT EXISTS snapshot (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			action_hash TEXT NOT NULL,
			algorithm_version TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS intervals (
			interval_id INTEGER PRIMARY KEY,
			frame_start INTEGER NOT NULL,
			frame_end INTEGER NOT NULL,
			state TEXT NOT NULL,
			payload BLOB NOT NULL,
			content_hash TEXT NOT NULL,
			summary_json BLOB NOT NULL,
			deps_json BLOB NOT NULL,
			dependency_hash TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			topology_version TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			state_progress INTEGER NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	var version string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		return err
	}
	if version != SchemaVersion {
		return &model.Error{Code: model.CodeState, Frame: -1, Message: "unsupported checkpoint schema version " + version}
	}
	return nil
}

func (s *Store) Begin(ctx context.Context, meta Meta) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actionHash, algorithm string
	err = tx.QueryRowContext(ctx, `SELECT action_hash, algorithm_version FROM snapshot WHERE id=1`).Scan(&actionHash, &algorithm)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot(id, action_hash, algorithm_version) VALUES(1, ?, ?)`, meta.ActionHash, meta.AlgorithmVersion); err != nil {
			return err
		}
	case err != nil:
		return err
	case actionHash != meta.ActionHash || algorithm != meta.AlgorithmVersion:
		if _, err := tx.ExecContext(ctx, `DELETE FROM intervals`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE snapshot SET action_hash=?, algorithm_version=? WHERE id=1`, meta.ActionHash, meta.AlgorithmVersion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CommitInterval(ctx context.Context, record IntervalRecord) (IntervalRecord, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if record.IntervalID < 0 || record.FrameEnd < record.FrameStart {
		return record, &model.Error{Code: model.CodeState, Frame: -1, Message: "invalid interval commit bounds"}
	}
	if record.State != "committed" {
		return record, &model.Error{Code: model.CodeState, Frame: -1, Message: "only committed intervals may be checkpointed"}
	}
	if record.AlgorithmVersion == "" || record.ParamVersion == "" || record.TopologyVersion == "" || record.CacheKey == "" || record.DependencyHash == "" {
		return record, &model.Error{Code: model.CodeState, Frame: -1, Message: "interval versions are required"}
	}
	sum := sha256.Sum256(record.Payload)
	record.ContentHash = hex.EncodeToString(sum[:])
	deps, err := json.Marshal(record.Dependencies)
	if err != nil {
		return record, err
	}
	if len(record.Summary) == 0 {
		record.Summary = json.RawMessage(`{}`)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return record, err
	}
	defer tx.Rollback()
	var last int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(interval_id), -1) FROM intervals`).Scan(&last)
	if err != nil {
		return record, err
	}
	if record.IntervalID <= last {
		return record, &model.Error{
			Code: model.CodeState, Frame: record.FrameStart,
			Message: fmt.Sprintf("interval commit order is not stable: next=%d last=%d", record.IntervalID, last),
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO intervals(
		interval_id, frame_start, frame_end, state, payload, content_hash, summary_json,
		deps_json, dependency_hash, cache_key, topology_version, param_version, algorithm_version, state_progress
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.IntervalID, record.FrameStart, record.FrameEnd, record.State, record.Payload,
		record.ContentHash, []byte(record.Summary), deps, record.DependencyHash, record.CacheKey, record.TopologyVersion,
		record.ParamVersion, record.AlgorithmVersion, record.StateProgress)
	if err != nil {
		return record, err
	}
	if err := tx.Commit(); err != nil {
		return record, err
	}
	return record, nil
}

// Resume verifies the snapshot and every interval payload. A failing interval
// and all later intervals are removed atomically; the consistent prefix is
// returned to the caller.
func (s *Store) Resume(ctx context.Context, meta Meta, expected func(IntervalRecord) bool) (*ResumeResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var actionHash, algorithm string
	err := s.db.QueryRowContext(ctx, `SELECT action_hash, algorithm_version FROM snapshot WHERE id=1`).Scan(&actionHash, &algorithm)
	if errors.Is(err, sql.ErrNoRows) {
		return &ResumeResult{}, nil
	}
	if err != nil {
		return nil, err
	}
	if actionHash != meta.ActionHash || algorithm != meta.AlgorithmVersion {
		if err := s.deleteFromLocked(ctx, 0); err != nil {
			return nil, err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE snapshot SET action_hash=?, algorithm_version=? WHERE id=1`, meta.ActionHash, meta.AlgorithmVersion); err != nil {
			return nil, err
		}
		return &ResumeResult{InvalidatedFrom: 0, Invalidated: true, Reason: "snapshot action hash or algorithm version mismatch"}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT interval_id, frame_start, frame_end, state, payload,
		content_hash, summary_json, deps_json, dependency_hash, cache_key, topology_version, param_version,
		algorithm_version, state_progress FROM intervals ORDER BY interval_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []IntervalRecord
	invalidAt := -1
	reason := ""
	for rows.Next() {
		var record IntervalRecord
		var summary, deps []byte
		if err := rows.Scan(&record.IntervalID, &record.FrameStart, &record.FrameEnd, &record.State,
			&record.Payload, &record.ContentHash, &summary, &deps, &record.DependencyHash, &record.CacheKey, &record.TopologyVersion,
			&record.ParamVersion, &record.AlgorithmVersion, &record.StateProgress); err != nil {
			return nil, err
		}
		record.Summary = json.RawMessage(summary)
		if err := json.Unmarshal(deps, &record.Dependencies); err != nil {
			invalidAt, reason = record.IntervalID, "dependency metadata is corrupt"
		}
		sum := sha256.Sum256(record.Payload)
		if invalidAt < 0 && hex.EncodeToString(sum[:]) != record.ContentHash {
			invalidAt, reason = record.IntervalID, "content hash mismatch"
		}
		if invalidAt < 0 && record.State != "committed" {
			invalidAt, reason = record.IntervalID, "interval was not committed"
		}
		if invalidAt < 0 && expected != nil && !expected(record) {
			invalidAt, reason = record.IntervalID, "interval version or dependency mismatch"
		}
		if invalidAt < 0 {
			records = append(records, record)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if invalidAt >= 0 {
		if err := s.deleteFromLocked(ctx, invalidAt); err != nil {
			return nil, err
		}
		return &ResumeResult{Records: records, InvalidatedFrom: invalidAt, Invalidated: true, Reason: reason}, nil
	}
	return &ResumeResult{Records: records}, nil
}

func (s *Store) InvalidateFrom(ctx context.Context, intervalID int, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = reason
	return s.deleteFromLocked(ctx, intervalID)
}

func (s *Store) deleteFromLocked(ctx context.Context, intervalID int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM intervals WHERE interval_id >= ?`, intervalID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Reset(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM intervals`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

// CorruptPayload is intentionally narrow and exists so recovery tests can prove
// that a bad snapshot suffix is discarded without touching a good prefix.
func (s *Store) CorruptPayload(ctx context.Context, intervalID int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE intervals SET payload = payload || X'00' WHERE interval_id=?`, intervalID)
	return err
}
