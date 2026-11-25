package vpcompose

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

type CheckpointRecord struct {
	WindowIndex      int            `json:"windowIndex"`
	FrameStart       int            `json:"frameStart"`
	FrameEnd         int            `json:"frameEnd"`
	State            WindowState    `json:"state"`
	Snapshot         *StackSnapshot `json:"snapshot"`
	Result           *WindowResult  `json:"result"`
	Digest           string         `json:"digest"`
	ContentHash      string         `json:"contentHash"`
	SourceVersion    Version        `json:"sourceVersion"`
	ParamVersion     uint64         `json:"paramVersion"`
	AlgorithmVersion string         `json:"algorithmVersion"`
	OverallVersion   string         `json:"overallVersion"`
}

type CheckpointStore struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, &EngineError{Code: CodeDecode, Path: path, Message: "open sqlite checkpoint", Cause: err}
	}
	db.SetMaxOpenConns(1)
	store := &CheckpointStore{db: db, path: path}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) initialize() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS windows (
			window_index INTEGER PRIMARY KEY,
			frame_start INTEGER NOT NULL,
			frame_end INTEGER NOT NULL,
			commit_order INTEGER NOT NULL,
			state TEXT NOT NULL,
			payload BLOB NOT NULL,
			row_digest TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			source_version INTEGER NOT NULL,
			param_version INTEGER NOT NULL,
			algorithm_version TEXT NOT NULL,
			overall_version TEXT NOT NULL,
			snapshot_digest TEXT NOT NULL,
			result_digest TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return &EngineError{Code: CodeDecode, Path: s.path, Message: "initialize checkpoint schema", Cause: err}
		}
	}
	return nil
}

func (s *CheckpointStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func checkpointRowDigest(record *CheckpointRecord) (string, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return "", decodeError("checkpoint", err)
	}
	h := sha256.New()
	h.Write(payload)
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d|%d|%d|%s|%s|%d|%d|%s|%s|%s|%s",
		record.WindowIndex, record.FrameStart, record.FrameEnd, record.State,
		record.ContentHash, record.SourceVersion, record.ParamVersion,
		record.AlgorithmVersion, record.OverallVersion,
		record.Snapshot.Digest(), record.Result.Digest)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SaveCommitted atomically writes one committed window. A single mutex and one
// database connection serialize commits; the row carries both its own digest
// and the project-wide version identity.
func (s *CheckpointStore) SaveCommitted(record *CheckpointRecord) error {
	if s == nil || s.db == nil {
		return schemaError("checkpoint", "store is closed")
	}
	if record == nil || record.State != WindowCommitted || record.Snapshot == nil || record.Result == nil {
		return schemaError("checkpoint.record", "only complete committed windows can be stored")
	}
	if record.Result.Digest != record.Digest {
		return schemaError("checkpoint.digest", "result digest does not match record digest")
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return decodeError("checkpoint", err)
	}
	rowDigest, err := checkpointRowDigest(record)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return &EngineError{Code: CodeDecode, Path: s.path, Message: "begin checkpoint transaction", Cause: err}
	}
	defer func() { _ = tx.Rollback() }()
	var storedVersion string
	err = tx.QueryRow(`SELECT value FROM meta WHERE key='overall_version'`).Scan(&storedVersion)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('overall_version', ?)`, record.OverallVersion); err != nil {
			return &EngineError{Code: CodeDecode, Path: s.path, Message: "write checkpoint metadata", Cause: err}
		}
	case err != nil:
		return &EngineError{Code: CodeDecode, Path: s.path, Message: "read checkpoint metadata", Cause: err}
	case storedVersion != record.OverallVersion:
		return &EngineError{
			Code: CodeState, Path: "checkpoint.overallVersion",
			Message:  "overall version changed while committing",
			Expected: storedVersion, Actual: record.OverallVersion,
		}
	}
	_, err = tx.Exec(`INSERT INTO windows(
		window_index, frame_start, frame_end, commit_order, state, payload, row_digest,
		content_hash, source_version, param_version, algorithm_version, overall_version,
		snapshot_digest, result_digest
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(window_index) DO UPDATE SET
		frame_start=excluded.frame_start,
		frame_end=excluded.frame_end,
		commit_order=excluded.commit_order,
		state=excluded.state,
		payload=excluded.payload,
		row_digest=excluded.row_digest,
		content_hash=excluded.content_hash,
		source_version=excluded.source_version,
		param_version=excluded.param_version,
		algorithm_version=excluded.algorithm_version,
		overall_version=excluded.overall_version,
		snapshot_digest=excluded.snapshot_digest,
		result_digest=excluded.result_digest`,
		record.WindowIndex, record.FrameStart, record.FrameEnd, record.WindowIndex,
		string(record.State), payload, rowDigest, record.ContentHash,
		int(record.SourceVersion), int64(record.ParamVersion), record.AlgorithmVersion,
		record.OverallVersion, record.Snapshot.Digest(), record.Result.Digest)
	if err != nil {
		return &EngineError{Code: CodeDecode, Path: s.path, Message: "write checkpoint window", Cause: err}
	}
	if err := tx.Commit(); err != nil {
		return &EngineError{Code: CodeDecode, Path: s.path, Message: "commit checkpoint transaction", Cause: err}
	}
	return nil
}

// LoadCommitted returns only committed windows. Any whole-version mismatch or
// damaged row causes the complete snapshot to be erased atomically.
func (s *CheckpointStore) LoadCommitted(expectedOverallVersion string) ([]CheckpointRecord, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, schemaError("checkpoint", "store is closed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, &EngineError{Code: CodeDecode, Path: s.path, Message: "begin checkpoint read", Cause: err}
	}
	defer func() { _ = tx.Rollback() }()
	var storedVersion string
	err = tx.QueryRow(`SELECT value FROM meta WHERE key='overall_version'`).Scan(&storedVersion)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, &EngineError{Code: CodeDecode, Path: s.path, Message: "read checkpoint metadata", Cause: err}
	}
	wipe := func() ([]CheckpointRecord, bool, error) {
		if _, err := tx.Exec(`DELETE FROM windows`); err != nil {
			return nil, false, &EngineError{Code: CodeDecode, Path: s.path, Message: "erase invalid checkpoint rows", Cause: err}
		}
		if _, err := tx.Exec(`DELETE FROM meta WHERE key='overall_version'`); err != nil {
			return nil, false, &EngineError{Code: CodeDecode, Path: s.path, Message: "erase invalid checkpoint metadata", Cause: err}
		}
		if err := tx.Commit(); err != nil {
			return nil, false, &EngineError{Code: CodeDecode, Path: s.path, Message: "commit checkpoint invalidation", Cause: err}
		}
		return nil, true, nil
	}
	if storedVersion != expectedOverallVersion {
		return wipe()
	}
	rows, err := tx.Query(`SELECT window_index, payload, row_digest, content_hash,
		source_version, param_version, algorithm_version, overall_version,
		snapshot_digest, result_digest, state
		FROM windows ORDER BY window_index ASC`)
	if err != nil {
		return nil, false, &EngineError{Code: CodeDecode, Path: s.path, Message: "query checkpoint windows", Cause: err}
	}
	defer rows.Close()
	var records []CheckpointRecord
	for rows.Next() {
		var (
			index                                                    int
			payload                                                  []byte
			rowDigest, contentHash, algorithmVersion, overallVersion string
			snapshotDigest, resultDigest, state                      string
			sourceVersion                                            int
			paramVersion                                             int64
		)
		if err := rows.Scan(&index, &payload, &rowDigest, &contentHash, &sourceVersion,
			&paramVersion, &algorithmVersion, &overallVersion, &snapshotDigest,
			&resultDigest, &state); err != nil {
			rows.Close()
			return wipe()
		}
		var record CheckpointRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			rows.Close()
			return wipe()
		}
		if record.WindowIndex != index || record.ContentHash != contentHash ||
			int(record.SourceVersion) != sourceVersion || int64(record.ParamVersion) != paramVersion ||
			record.AlgorithmVersion != algorithmVersion || record.OverallVersion != overallVersion ||
			record.State != WindowCommitted || state != string(WindowCommitted) ||
			record.Snapshot == nil || record.Result == nil ||
			record.Snapshot.WindowIndex != record.WindowIndex ||
			record.Snapshot.ContentHash != record.ContentHash ||
			record.Snapshot.ParamVersion != record.ParamVersion ||
			record.Snapshot.SourceVersion != record.SourceVersion ||
			record.Snapshot.AlgorithmVersion != record.AlgorithmVersion ||
			record.Snapshot.Digest() != snapshotDigest || record.Result.Digest != resultDigest {
			rows.Close()
			return wipe()
		}
		computed, err := checkpointRowDigest(&record)
		if err != nil || computed != rowDigest {
			rows.Close()
			return wipe()
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return wipe()
	}
	if err := tx.Commit(); err != nil {
		return nil, false, &EngineError{Code: CodeDecode, Path: s.path, Message: "finish checkpoint read", Cause: err}
	}
	return records, false, nil
}

func (s *CheckpointStore) Reset() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return &EngineError{Code: CodeDecode, Path: s.path, Message: "begin checkpoint reset", Cause: err}
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM windows`); err != nil {
		return &EngineError{Code: CodeDecode, Path: s.path, Message: "clear checkpoint windows", Cause: err}
	}
	if _, err := tx.Exec(`DELETE FROM meta WHERE key='overall_version'`); err != nil {
		return &EngineError{Code: CodeDecode, Path: s.path, Message: "clear checkpoint metadata", Cause: err}
	}
	if err := tx.Commit(); err != nil {
		return &EngineError{Code: CodeDecode, Path: s.path, Message: "commit checkpoint reset", Cause: err}
	}
	return nil
}
