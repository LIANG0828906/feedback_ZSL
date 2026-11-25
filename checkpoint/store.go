package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"skillbudget"
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
	path    string
}

type StoredWindow struct {
	RunID            string
	Index            int
	StartTick        int64
	EndTick          int64
	Status           skillcore.WindowState
	RuleHash         string
	SourceVersion    int
	AlgorithmVersion string
	Attempt          int
	StateBeforeJSON  string
	StateAfterJSON   string
	ResultJSON       string
	DecisionDigest   string
	ErrorText        string
	UpdatedAtMS      int64
}

type CacheEntry struct {
	Key            string
	StateDigest    string
	Algorithm      string
	RuleHash       string
	ResultJSON     string
	StateAfterJSON string
	UpdatedAtMS    int64
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, &skillcore.Error{
			Code: skillcore.CodeInput, Path: "database path", Field: "database path",
			Message: "must not be empty",
		}
	}
	if path != ":memory:" && path != "file::memory:" {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create database directory: %w", err)
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection makes transaction ordering and commit serialization
	// explicit and avoids SQLITE_BUSY interleaving inside one process.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS checkpoint_windows (
			run_id TEXT NOT NULL,
			window_index INTEGER NOT NULL,
			start_tick INTEGER NOT NULL,
			end_tick INTEGER NOT NULL,
			status TEXT NOT NULL,
			rule_hash TEXT NOT NULL,
			source_version INTEGER NOT NULL,
			algorithm_version TEXT NOT NULL,
			attempt INTEGER NOT NULL DEFAULT 0,
			state_before TEXT NOT NULL DEFAULT '',
			state_after TEXT NOT NULL DEFAULT '',
			result_json TEXT NOT NULL DEFAULT '',
			decision_digest TEXT NOT NULL DEFAULT '',
			error_text TEXT NOT NULL DEFAULT '',
			updated_at_ms INTEGER NOT NULL,
			PRIMARY KEY (run_id, window_index)
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_cache (
			dependency_key TEXT PRIMARY KEY,
			state_digest TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			rule_hash TEXT NOT NULL,
			result_json TEXT NOT NULL,
			state_after TEXT NOT NULL,
			updated_at_ms INTEGER NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }

func (s *Store) EnsureWindow(ctx context.Context, runID string, index int, start, end int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO checkpoint_windows
		(run_id, window_index, start_tick, end_tick, status, rule_hash, source_version,
		 algorithm_version, attempt, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, '', 0, '', 0, ?)`,
		runID, index, start, end, string(skillcore.WindowPending), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("ensure window: %w", err)
	}
	return nil
}

type BeginWindowOptions struct {
	RunID            string
	Index            int
	StartTick        int64
	EndTick          int64
	RuleHash         string
	SourceVersion    int
	AlgorithmVersion string
	StateBeforeJSON  string
}

func (s *Store) BeginWindow(ctx context.Context, opts BeginWindowOptions) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin window transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	var attempt int
	err = tx.QueryRowContext(ctx, `SELECT status, attempt FROM checkpoint_windows
		WHERE run_id=? AND window_index=?`, opts.RunID, opts.Index).Scan(&status, &attempt)
	if err == sql.ErrNoRows {
		_, err = tx.ExecContext(ctx, `INSERT INTO checkpoint_windows
			(run_id, window_index, start_tick, end_tick, status, rule_hash, source_version,
			 algorithm_version, attempt, updated_at_ms)
			VALUES (?, ?, ?, ?, ?, '', 0, '', 0, ?)`,
			opts.RunID, opts.Index, opts.StartTick, opts.EndTick,
			string(skillcore.WindowPending), time.Now().UnixMilli())
		if err != nil {
			return fmt.Errorf("insert pending window: %w", err)
		}
		status = string(skillcore.WindowPending)
		attempt = 0
	} else if err != nil {
		return fmt.Errorf("read window status: %w", err)
	}
	current := skillcore.WindowState(status)
	if !skillcore.CanTransitionWindow(current, skillcore.WindowSettling) {
		return &skillcore.Error{
			Code:         skillcore.CodeState,
			Path:         fmt.Sprintf("windows[%d]", opts.Index),
			Message:      fmt.Sprintf("cannot begin window from %s", current),
			CurrentState: string(current),
			NextState:    string(skillcore.WindowSettling),
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE checkpoint_windows SET
		start_tick=?, end_tick=?, status=?, rule_hash=?, source_version=?,
		algorithm_version=?, attempt=?, state_before=?, state_after='', result_json='',
		decision_digest='', error_text='', updated_at_ms=?
		WHERE run_id=? AND window_index=?`,
		opts.StartTick, opts.EndTick, string(skillcore.WindowSettling), opts.RuleHash,
		opts.SourceVersion, opts.AlgorithmVersion, attempt+1, opts.StateBeforeJSON,
		time.Now().UnixMilli(), opts.RunID, opts.Index)
	if err != nil {
		return fmt.Errorf("begin window: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return fmt.Errorf("begin window: expected one row, updated %d", rows)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit begin window: %w", err)
	}
	return nil
}

type CommitWindowOptions struct {
	RunID          string
	Index          int
	StateAfterJSON string
	ResultJSON     string
	DecisionDigest string
}

func (s *Store) CommitWindow(ctx context.Context, opts CommitWindowOptions) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin commit transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM checkpoint_windows
		WHERE run_id=? AND window_index=?`, opts.RunID, opts.Index).Scan(&status)
	if err != nil {
		return fmt.Errorf("read commit status: %w", err)
	}
	current := skillcore.WindowState(status)
	if !skillcore.CanTransitionWindow(current, skillcore.WindowCommitted) {
		return &skillcore.Error{
			Code:         skillcore.CodeState,
			Path:         fmt.Sprintf("windows[%d]", opts.Index),
			Message:      fmt.Sprintf("cannot commit window from %s", current),
			CurrentState: string(current),
			NextState:    string(skillcore.WindowCommitted),
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE checkpoint_windows SET
		status=?, state_after=?, result_json=?, decision_digest=?, error_text='', updated_at_ms=?
		WHERE run_id=? AND window_index=? AND status=?`,
		string(skillcore.WindowCommitted), opts.StateAfterJSON, opts.ResultJSON,
		opts.DecisionDigest, time.Now().UnixMilli(), opts.RunID, opts.Index,
		string(skillcore.WindowSettling))
	if err != nil {
		return fmt.Errorf("commit window: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return &skillcore.Error{
			Code:    skillcore.CodeState,
			Path:    fmt.Sprintf("windows[%d]", opts.Index),
			Message: "window changed state concurrently; commit rejected",
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit window transaction: %w", err)
	}
	return nil
}

func (s *Store) FailWindow(ctx context.Context, runID string, index int, message string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin fail transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM checkpoint_windows
		WHERE run_id=? AND window_index=?`, runID, index).Scan(&status)
	if err != nil {
		return fmt.Errorf("read failed window status: %w", err)
	}
	current := skillcore.WindowState(status)
	if !skillcore.CanTransitionWindow(current, skillcore.WindowFailed) {
		return &skillcore.Error{
			Code:         skillcore.CodeState,
			Path:         fmt.Sprintf("windows[%d]", index),
			Message:      fmt.Sprintf("cannot fail window from %s", current),
			CurrentState: string(current),
			NextState:    string(skillcore.WindowFailed),
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE checkpoint_windows SET
		status=?, error_text=?, updated_at_ms=? WHERE run_id=? AND window_index=? AND status=?`,
		string(skillcore.WindowFailed), message, time.Now().UnixMilli(), runID, index,
		string(skillcore.WindowSettling))
	if err != nil {
		return fmt.Errorf("fail window: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return &skillcore.Error{
			Code:    skillcore.CodeState,
			Path:    fmt.Sprintf("windows[%d]", index),
			Message: "window changed state concurrently; failure transition rejected",
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fail transaction: %w", err)
	}
	return nil
}

// RecoverInterrupted is the only settling -> failed recovery path used after a
// process restart. Failed windows may then be retried legally.
func (s *Store) RecoverInterrupted(ctx context.Context, runID string) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE checkpoint_windows SET
		status=?, error_text='process restarted while settling', updated_at_ms=?
		WHERE run_id=? AND status=?`,
		string(skillcore.WindowFailed), time.Now().UnixMilli(), runID,
		string(skillcore.WindowSettling))
	if err != nil {
		return 0, fmt.Errorf("recover interrupted windows: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) LoadWindows(ctx context.Context, runID string) ([]StoredWindow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, window_index, start_tick, end_tick,
		status, rule_hash, source_version, algorithm_version, attempt, state_before,
		state_after, result_json, decision_digest, error_text, updated_at_ms
		FROM checkpoint_windows WHERE run_id=? ORDER BY window_index`, runID)
	if err != nil {
		return nil, fmt.Errorf("load windows: %w", err)
	}
	defer rows.Close()
	var out []StoredWindow
	for rows.Next() {
		var item StoredWindow
		var status string
		if err := rows.Scan(&item.RunID, &item.Index, &item.StartTick, &item.EndTick,
			&status, &item.RuleHash, &item.SourceVersion, &item.AlgorithmVersion,
			&item.Attempt, &item.StateBeforeJSON, &item.StateAfterJSON,
			&item.ResultJSON, &item.DecisionDigest, &item.ErrorText, &item.UpdatedAtMS); err != nil {
			return nil, fmt.Errorf("scan window: %w", err)
		}
		item.Status = skillcore.WindowState(status)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate windows: %w", err)
	}
	return out, nil
}

func (s *Store) WindowState(ctx context.Context, runID string, index int) (skillcore.WindowState, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM checkpoint_windows
		WHERE run_id=? AND window_index=?`, runID, index).Scan(&status)
	if err == sql.ErrNoRows {
		return "", &skillcore.Error{
			Code: skillcore.CodeState, Path: fmt.Sprintf("windows[%d]", index), Message: "window not found",
		}
	}
	if err != nil {
		return "", err
	}
	return skillcore.WindowState(status), nil
}

func (s *Store) ResetRun(ctx context.Context, runID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reset transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_windows WHERE run_id=?`, runID); err != nil {
		return fmt.Errorf("delete run windows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reset transaction: %w", err)
	}
	return nil
}

func (s *Store) PutCachedWindow(ctx context.Context, entry CacheEntry) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if entry.UpdatedAtMS == 0 {
		entry.UpdatedAtMS = time.Now().UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO checkpoint_cache
		(dependency_key, state_digest, algorithm_version, rule_hash, result_json, state_after, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(dependency_key) DO UPDATE SET
			state_digest=excluded.state_digest,
			algorithm_version=excluded.algorithm_version,
			rule_hash=excluded.rule_hash,
			result_json=excluded.result_json,
			state_after=excluded.state_after,
			updated_at_ms=excluded.updated_at_ms`,
		entry.Key, entry.StateDigest, entry.Algorithm, entry.RuleHash,
		entry.ResultJSON, entry.StateAfterJSON, entry.UpdatedAtMS)
	if err != nil {
		return fmt.Errorf("put window cache: %w", err)
	}
	return nil
}

func (s *Store) GetCachedWindow(ctx context.Context, key, stateDigest, algorithm string) (*CacheEntry, bool, error) {
	var entry CacheEntry
	err := s.db.QueryRowContext(ctx, `SELECT dependency_key, state_digest, algorithm_version,
		rule_hash, result_json, state_after, updated_at_ms
		FROM checkpoint_cache WHERE dependency_key=?`, key).
		Scan(&entry.Key, &entry.StateDigest, &entry.Algorithm, &entry.RuleHash,
			&entry.ResultJSON, &entry.StateAfterJSON, &entry.UpdatedAtMS)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get window cache: %w", err)
	}
	if entry.StateDigest != stateDigest || entry.Algorithm != algorithm {
		return nil, false, nil
	}
	return &entry, true, nil
}

func (s *Store) ClearCache(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoint_cache`); err != nil {
		return fmt.Errorf("clear cache: %w", err)
	}
	return nil
}

func marshalState(state *skillcore.RuntimeState) (string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalState(data string) (*skillcore.RuntimeState, error) {
	var state skillcore.RuntimeState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		return nil, err
	}
	return &state, nil
}
