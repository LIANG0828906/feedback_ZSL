// Package checkpoint provides serialized, atomic SQLite window checkpoints.
package checkpoint

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"economycore/model"

	_ "modernc.org/sqlite"
)

// Record is a committed settlement window.
type Record struct {
	Window       int               `json:"window"`
	IntentOffset int               `json:"intentOffset"`
	NextCycle    int64             `json:"nextCycle"`
	StateJSON    []byte            `json:"-"`
	ResultJSON   []byte            `json:"-"`
	Fingerprint  model.Fingerprint `json:"fingerprint"`
	CommittedAt  string            `json:"committedAt"`
}

// Failure is an isolated failed window attempt.
type Failure struct {
	Window      int               `json:"window"`
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	FailedAt    string            `json:"failedAt"`
	Fingerprint model.Fingerprint `json:"fingerprint"`
}

// Recovery reports the valid contiguous checkpoint prefix.
type Recovery struct {
	Fingerprint        model.Fingerprint `json:"fingerprint"`
	ExpectedWindows    int               `json:"expectedWindows"`
	ValidWindows       int               `json:"validWindows"`
	LastWindow         int               `json:"lastWindow"`
	Complete           bool              `json:"complete"`
	Invalidated        bool              `json:"invalidated"`
	InvalidationReason string            `json:"invalidationReason,omitempty"`
	Records            []Record          `json:"records,omitempty"`
	Failures           []Failure         `json:"failures,omitempty"`
}

// Store serializes all writes through one connection and one mutex.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// Open opens or creates the SQLite checkpoint database.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, model.NewError(model.EStore, "/database", "empty database path")
	}
	if path != ":memory:" {
		if dir := directory(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, model.NewError(model.EIO, "/database", err.Error()).WithCause(err)
			}
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	if path == ":memory:" {
		dsn = ":memory:"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, model.NewError(model.EStore, "/database", err.Error()).WithCause(err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, model.NewError(model.EStore, "/database", "enable WAL: "+err.Error()).WithCause(err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=FULL`); err != nil {
		db.Close()
		return nil, model.NewError(model.EStore, "/database", "set synchronous: "+err.Error()).WithCause(err)
	}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func directory(path string) string {
	last := -1
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			last = i
			break
		}
	}
	if last <= 0 {
		return ""
	}
	return path[:last]
}

func (s *Store) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoints (
			window_no INTEGER PRIMARY KEY CHECK (window_no >= 0),
			intent_offset INTEGER NOT NULL,
			next_cycle INTEGER NOT NULL,
			state_json BLOB NOT NULL,
			result_json BLOB NOT NULL,
			content_hash TEXT NOT NULL,
			source_version TEXT NOT NULL,
			conversion_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			committed_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS window_failures (
			window_no INTEGER PRIMARY KEY CHECK (window_no >= 0),
			code TEXT NOT NULL,
			message TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			source_version TEXT NOT NULL,
			conversion_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			failed_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return model.NewError(model.EStore, "/schema", err.Error()).WithCause(err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

// CommitWindow atomically appends one window. Existing identical windows are
// idempotent; gaps and version mixes are rejected.
func (s *Store) CommitWindow(ctx context.Context, record Record) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.Window < 0 {
		return false, model.NewError(model.EWindow, "/window", "negative window").WithWindow(record.Window)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, storeError("begin checkpoint transaction", err)
	}
	defer tx.Rollback()

	existing, err := scanRecord(tx.QueryRowContext(ctx, `SELECT window_no, intent_offset, next_cycle, state_json, result_json,
		content_hash, source_version, conversion_version, algorithm_version, committed_at FROM checkpoints WHERE window_no = ?`, record.Window))
	if err != nil && err != sql.ErrNoRows {
		return false, storeError("read checkpoint", err)
	}
	if err == nil {
		if err := model.ValidateFingerprint(existing.Fingerprint, record.Fingerprint); err != nil {
			return false, model.NewError(model.EVer, "/checkpoint", "existing window uses a different version identity").WithWindow(record.Window).WithCause(err)
		}
		if err := tx.Commit(); err != nil {
			return false, storeError("commit idempotent checkpoint", err)
		}
		return true, nil
	}

	var count, maxWindow int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(window_no), -1) FROM checkpoints`).Scan(&count, &maxWindow); err != nil {
		return false, storeError("inspect checkpoint sequence", err)
	}
	if count != maxWindow+1 {
		return false, model.NewError(model.EStore, "/checkpoints", "checkpoint sequence contains a gap").WithWindow(record.Window)
	}
	expected := maxWindow + 1
	if record.Window != expected {
		return false, model.NewError(model.EWindow, "/checkpoints", fmt.Sprintf("expected window %d, got %d", expected, record.Window)).WithWindow(record.Window)
	}
	var mismatches int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints
		WHERE content_hash <> ? OR source_version <> ? OR conversion_version <> ? OR algorithm_version <> ?`,
		record.Fingerprint.ContentHash, record.Fingerprint.SourceVersion,
		record.Fingerprint.ConversionVersion, record.Fingerprint.AlgorithmVersion).Scan(&mismatches); err != nil {
		return false, storeError("check fingerprint compatibility", err)
	}
	if mismatches != 0 {
		return false, model.NewError(model.EVer, "/checkpoints", "refusing to mix checkpoint identities").WithWindow(record.Window)
	}
	committedAt := record.CommittedAt
	if committedAt == "" {
		committedAt = model.NowUTC().Format(time.RFC3339Nano)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints
		(window_no, intent_offset, next_cycle, state_json, result_json, content_hash, source_version, conversion_version, algorithm_version, committed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.Window, record.IntentOffset, record.NextCycle, record.StateJSON, record.ResultJSON,
		record.Fingerprint.ContentHash, record.Fingerprint.SourceVersion,
		record.Fingerprint.ConversionVersion, record.Fingerprint.AlgorithmVersion, committedAt); err != nil {
		return false, storeError("insert checkpoint", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM window_failures WHERE window_no = ?`, record.Window); err != nil {
		return false, storeError("clear window failure", err)
	}
	if err := tx.Commit(); err != nil {
		return false, storeError("commit checkpoint", err)
	}
	return false, nil
}

func scanRecord(row *sql.Row) (Record, error) {
	var record Record
	err := row.Scan(&record.Window, &record.IntentOffset, &record.NextCycle, &record.StateJSON, &record.ResultJSON,
		&record.Fingerprint.ContentHash, &record.Fingerprint.SourceVersion,
		&record.Fingerprint.ConversionVersion, &record.Fingerprint.AlgorithmVersion, &record.CommittedAt)
	return record, err
}

// LoadCompatible validates the whole snapshot globally. A mismatch in any
// fingerprint field or a sequence gap invalidates every stored checkpoint.
func (s *Store) LoadCompatible(ctx context.Context, fingerprint model.Fingerprint, expectedWindows int) (Recovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recovery := Recovery{Fingerprint: fingerprint, ExpectedWindows: expectedWindows, LastWindow: -1}
	rows, err := s.db.QueryContext(ctx, `SELECT window_no, intent_offset, next_cycle, state_json, result_json,
		content_hash, source_version, conversion_version, algorithm_version, committed_at
		FROM checkpoints ORDER BY window_no ASC`)
	if err != nil {
		return recovery, storeError("load checkpoints", err)
	}
	for rows.Next() {
		var record Record
		if err := rows.Scan(&record.Window, &record.IntentOffset, &record.NextCycle, &record.StateJSON, &record.ResultJSON,
			&record.Fingerprint.ContentHash, &record.Fingerprint.SourceVersion,
			&record.Fingerprint.ConversionVersion, &record.Fingerprint.AlgorithmVersion, &record.CommittedAt); err != nil {
			rows.Close()
			return recovery, storeError("scan checkpoint", err)
		}
		recovery.Records = append(recovery.Records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return recovery, storeError("iterate checkpoints", err)
	}
	rows.Close()

	invalidReason := ""
	for index, record := range recovery.Records {
		if record.Window != index {
			invalidReason = fmt.Sprintf("window sequence gap at position %d: window %d", index, record.Window)
			break
		}
		if err := model.ValidateFingerprint(record.Fingerprint, fingerprint); err != nil {
			invalidReason = "checkpoint identity mismatch at window " + fmt.Sprint(record.Window)
			break
		}
	}
	if invalidReason != "" {
		if err := s.invalidateAllLocked(ctx); err != nil {
			return recovery, err
		}
		recovery = Recovery{
			Fingerprint:        fingerprint,
			ExpectedWindows:    expectedWindows,
			LastWindow:         -1,
			Invalidated:        true,
			InvalidationReason: invalidReason,
		}
		return recovery, nil
	}
	recovery.ValidWindows = len(recovery.Records)
	if recovery.ValidWindows > 0 {
		recovery.LastWindow = recovery.Records[recovery.ValidWindows-1].Window
	}
	recovery.Complete = expectedWindows >= 0 && recovery.ValidWindows == expectedWindows
	failures, err := s.failuresLocked(ctx)
	if err != nil {
		return recovery, err
	}
	for _, failure := range failures {
		if err := model.ValidateFingerprint(failure.Fingerprint, fingerprint); err != nil {
			if err := s.invalidateAllLocked(ctx); err != nil {
				return recovery, err
			}
			return Recovery{
				Fingerprint:        fingerprint,
				ExpectedWindows:    expectedWindows,
				LastWindow:         -1,
				Invalidated:        true,
				InvalidationReason: "failure identity mismatch at window " + fmt.Sprint(failure.Window),
			}, nil
		}
	}
	recovery.Failures = failures
	return recovery, nil
}

func (s *Store) invalidateAllLocked(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storeError("begin invalidation", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints`); err != nil {
		return storeError("clear checkpoints", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM window_failures`); err != nil {
		return storeError("clear window failures", err)
	}
	if err := tx.Commit(); err != nil {
		return storeError("commit invalidation", err)
	}
	return nil
}

// InvalidateAll atomically clears checkpoints and failures.
func (s *Store) InvalidateAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateAllLocked(ctx)
}

// RecordFailure stores a failed window without touching committed windows.
func (s *Store) RecordFailure(ctx context.Context, window int, fingerprint model.Fingerprint, code, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if window < 0 {
		return model.NewError(model.EWindow, "/window", "negative window").WithWindow(window)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO window_failures
		(window_no, code, message, content_hash, source_version, conversion_version, algorithm_version, failed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(window_no) DO UPDATE SET
			code=excluded.code, message=excluded.message, content_hash=excluded.content_hash,
			source_version=excluded.source_version, conversion_version=excluded.conversion_version,
			algorithm_version=excluded.algorithm_version, failed_at=excluded.failed_at`,
		window, code, message, fingerprint.ContentHash, fingerprint.SourceVersion,
		fingerprint.ConversionVersion, fingerprint.AlgorithmVersion, model.NowUTC().Format(time.RFC3339Nano))
	if err != nil {
		return storeError("record window failure", err)
	}
	return nil
}

// Failures returns isolated failures for the current database.
func (s *Store) Failures(ctx context.Context) ([]Failure, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failuresLocked(ctx)
}

func (s *Store) failuresLocked(ctx context.Context) ([]Failure, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT window_no, code, message, content_hash, source_version,
		conversion_version, algorithm_version, failed_at FROM window_failures ORDER BY window_no ASC`)
	if err != nil {
		return nil, storeError("load failures", err)
	}
	defer rows.Close()
	var out []Failure
	for rows.Next() {
		var failure Failure
		if err := rows.Scan(&failure.Window, &failure.Code, &failure.Message,
			&failure.Fingerprint.ContentHash, &failure.Fingerprint.SourceVersion,
			&failure.Fingerprint.ConversionVersion, &failure.Fingerprint.AlgorithmVersion,
			&failure.FailedAt); err != nil {
			return nil, storeError("scan failure", err)
		}
		out = append(out, failure)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError("iterate failures", err)
	}
	return out, nil
}

// Count returns the number of committed windows.
func (s *Store) Count(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints`).Scan(&count); err != nil {
		return 0, storeError("count checkpoints", err)
	}
	return count, nil
}

func storeError(operation string, err error) error {
	return model.NewError(model.EStore, "/database", operation+": "+err.Error()).WithCause(err)
}
