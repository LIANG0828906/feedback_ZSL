package macrokit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db  *sql.DB
	mu  sync.Mutex
	dsn string
}

func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	if path == "" {
		path = ":memory:"
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLiteStore{db: db, dsn: path}
	if err := store.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) initSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS frame_windows (
  project_id TEXT NOT NULL,
  window_index INTEGER NOT NULL,
  start_frame INTEGER NOT NULL,
  end_frame INTEGER NOT NULL,
  consumed_offset INTEGER NOT NULL,
  format_no INTEGER NOT NULL,
  format_version TEXT NOT NULL,
  branch_results_json TEXT NOT NULL,
  condition_snapshots_json TEXT NOT NULL,
  execution_state_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'committed',
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(project_id, window_index)
);
CREATE TABLE IF NOT EXISTS result_cache (
  project_id TEXT NOT NULL,
  branch_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  PRIMARY KEY(project_id, branch_id)
);`)
	return err
}

type WindowCheckpoint struct {
	ProjectID              string                    `json:"project_id"`
	WindowIndex            int64                     `json:"window_index"`
	StartFrame             int64                     `json:"start_frame"`
	EndFrame               int64                     `json:"end_frame"`
	ConsumedOffset         int                       `json:"consumed_offset"`
	FormatNo               int                       `json:"format_no"`
	FormatVersion          string                    `json:"format_version"`
	BranchResults          map[string]*BranchState   `json:"-"`
	ConditionSnapshots     map[int64]map[string]bool `json:"-"`
	State                  *ExecutionState           `json:"-"`
	BranchResultsJSON      string                    `json:"branch_results_json"`
	ConditionSnapshotsJSON string                    `json:"condition_snapshots_json"`
	ExecutionStateJSON     string                    `json:"execution_state_json"`
	ContentHash            string                    `json:"content_hash"`
	Status                 string                    `json:"status"`
	ErrorCode              string                    `json:"error_code,omitempty"`
	ErrorMessage           string                    `json:"error_message,omitempty"`
	Attempt                int                       `json:"attempt"`
}

type windowHashPayload struct {
	ProjectID              string          `json:"project_id"`
	WindowIndex            int64           `json:"window_index"`
	StartFrame             int64           `json:"start_frame"`
	EndFrame               int64           `json:"end_frame"`
	ConsumedOffset         int             `json:"consumed_offset"`
	FormatNo               int             `json:"format_no"`
	FormatVersion          string          `json:"format_version"`
	BranchResultsJSON      json.RawMessage `json:"branch_results_json"`
	ConditionSnapshotsJSON json.RawMessage `json:"condition_snapshots_json"`
	ExecutionStateJSON     json.RawMessage `json:"execution_state_json"`
}

func (w *WindowCheckpoint) Prepare() error {
	var err error
	if w.BranchResultsJSON == "" {
		if w.BranchResultsJSON, err = marshalJSONObject(w.BranchResults); err != nil {
			return err
		}
	}
	if w.ConditionSnapshotsJSON == "" {
		if w.ConditionSnapshotsJSON, err = marshalJSONObject(w.ConditionSnapshots); err != nil {
			return err
		}
	}
	if w.ExecutionStateJSON == "" {
		if w.ExecutionStateJSON, err = marshalJSONObject(w.State); err != nil {
			return err
		}
	}
	w.ContentHash = w.computeHash()
	if w.Status == "" {
		w.Status = "committed"
	}
	return nil
}

func (w *WindowCheckpoint) computeHash() string {
	return HashCanonical(windowHashPayload{
		ProjectID: w.ProjectID, WindowIndex: w.WindowIndex, StartFrame: w.StartFrame, EndFrame: w.EndFrame,
		ConsumedOffset: w.ConsumedOffset, FormatNo: w.FormatNo, FormatVersion: w.FormatVersion,
		BranchResultsJSON: json.RawMessage(w.BranchResultsJSON), ConditionSnapshotsJSON: json.RawMessage(w.ConditionSnapshotsJSON),
		ExecutionStateJSON: json.RawMessage(w.ExecutionStateJSON),
	})
}

func (w *WindowCheckpoint) VerifyHash() error {
	if !json.Valid([]byte(w.BranchResultsJSON)) || !json.Valid([]byte(w.ConditionSnapshotsJSON)) || !json.Valid([]byte(w.ExecutionStateJSON)) {
		return &Error{Code: ECorrupt, BranchID: w.ProjectID, Frame: w.EndFrame, Field: "payload_json", Message: "checkpoint payload is not valid JSON"}
	}
	expected := w.computeHash()
	if expected != w.ContentHash {
		return &Error{Code: ECorrupt, BranchID: w.ProjectID, Frame: w.EndFrame, Field: "content_hash", Message: "checkpoint content hash mismatch"}
	}
	return nil
}

func marshalJSONObject(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	if len(b) == 0 || string(b) == "null" {
		return "{}", nil
	}
	return string(b), nil
}

type FaultInjector struct {
	mu           sync.Mutex
	FailAttempts map[int64]int `json:"fail_attempts"`
}

func (f *FaultInjector) ShouldFail(windowIndex int64) bool {
	if f == nil || f.FailAttempts == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	remaining := f.FailAttempts[windowIndex]
	if remaining <= 0 {
		return false
	}
	remaining--
	if remaining == 0 {
		delete(f.FailAttempts, windowIndex)
	} else {
		f.FailAttempts[windowIndex] = remaining
	}
	return true
}

func (s *SQLiteStore) CommitWindow(ctx context.Context, w *WindowCheckpoint, faults *FaultInjector) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if faults.ShouldFail(w.WindowIndex) {
		if err := s.markWindowFailedLocked(ctx, w, EFault, "injected window commit failure"); err != nil {
			return err
		}
		return &Error{Code: EFault, Frame: w.EndFrame, Field: "frame_window", Message: "injected failure before window commit", BranchID: w.ProjectID}
	}

	w.Status = "committed"
	w.ErrorCode = ""
	w.ErrorMessage = ""
	if err := w.Prepare(); err != nil {
		_ = s.markWindowFailedLocked(ctx, w, ESchema, err.Error())
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
INSERT INTO frame_windows
 (project_id,window_index,start_frame,end_frame,consumed_offset,format_no,format_version,
  branch_results_json,condition_snapshots_json,execution_state_json,content_hash,status,error_code,error_message,attempt)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(project_id,window_index) DO UPDATE SET
 start_frame=excluded.start_frame,end_frame=excluded.end_frame,consumed_offset=excluded.consumed_offset,
 format_no=excluded.format_no,format_version=excluded.format_version,
 branch_results_json=excluded.branch_results_json,condition_snapshots_json=excluded.condition_snapshots_json,
 execution_state_json=excluded.execution_state_json,content_hash=excluded.content_hash,
 status='committed',error_code='',error_message='',attempt=frame_windows.attempt+1`,
		w.ProjectID, w.WindowIndex, w.StartFrame, w.EndFrame, w.ConsumedOffset, w.FormatNo, w.FormatVersion,
		w.BranchResultsJSON, w.ConditionSnapshotsJSON, w.ExecutionStateJSON, w.ContentHash,
		"committed", "", "", w.Attempt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) MarkWindowFailed(ctx context.Context, w *WindowCheckpoint, code ErrorCode, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markWindowFailedLocked(ctx, w, code, message)
}

func (s *SQLiteStore) markWindowFailedLocked(ctx context.Context, w *WindowCheckpoint, code ErrorCode, message string) error {
	var attempt int
	err := s.db.QueryRowContext(ctx, `SELECT attempt FROM frame_windows WHERE project_id=? AND window_index=?`, w.ProjectID, w.WindowIndex).Scan(&attempt)
	if errors.Is(err, sql.ErrNoRows) {
		attempt = 0
	} else if err != nil {
		return err
	}
	attempt++
	w.Attempt = attempt
	_, err = s.db.ExecContext(ctx, `
INSERT INTO frame_windows
 (project_id,window_index,start_frame,end_frame,consumed_offset,format_no,format_version,
  branch_results_json,condition_snapshots_json,execution_state_json,content_hash,status,error_code,error_message,attempt)
VALUES (?,?,?,?,?,?,?,?,?,?,?,'failed',?,?,?)
ON CONFLICT(project_id,window_index) DO UPDATE SET
 status='failed',error_code=excluded.error_code,error_message=excluded.error_message,attempt=excluded.attempt`,
		w.ProjectID, w.WindowIndex, w.StartFrame, w.EndFrame, w.ConsumedOffset, w.FormatNo, w.FormatVersion,
		"{}", "{}", "{}", "failed-attempt", string(code), message, attempt)
	return err
}

type CheckpointSnapshot struct {
	ProjectID     string
	FormatNo      int
	FormatVersion string
	Windows       []WindowCheckpoint
	State         *ExecutionState
}

func (s *SQLiteStore) LoadSnapshot(ctx context.Context, projectID string, expectedFormatNo int, expectedVersion string) (*CheckpointSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx, `
SELECT project_id,window_index,start_frame,end_frame,consumed_offset,format_no,format_version,
       branch_results_json,condition_snapshots_json,execution_state_json,content_hash,status,error_code,error_message,attempt
FROM frame_windows
WHERE project_id=? AND status='committed'
ORDER BY window_index ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	windows := []WindowCheckpoint{}
	for rows.Next() {
		var w WindowCheckpoint
		if err := rows.Scan(&w.ProjectID, &w.WindowIndex, &w.StartFrame, &w.EndFrame, &w.ConsumedOffset,
			&w.FormatNo, &w.FormatVersion, &w.BranchResultsJSON, &w.ConditionSnapshotsJSON, &w.ExecutionStateJSON,
			&w.ContentHash, &w.Status, &w.ErrorCode, &w.ErrorMessage, &w.Attempt); err != nil {
			return nil, err
		}
		if expectedVersion != "" && (w.FormatNo != expectedFormatNo || w.FormatVersion != expectedVersion) {
			_ = rows.Close()
			_ = s.invalidateProjectLocked(ctx, projectID)
			return nil, &Error{Code: EVersion, BranchID: projectID, Frame: w.EndFrame, Field: "format_version", Message: fmt.Sprintf("checkpoint format %d/%s does not match current %d/%s", w.FormatNo, w.FormatVersion, expectedFormatNo, expectedVersion)}
		}
		if err := w.VerifyHash(); err != nil {
			_ = rows.Close()
			_ = s.invalidateProjectLocked(ctx, projectID)
			return nil, err
		}
		if err := json.Unmarshal([]byte(w.ExecutionStateJSON), &w.State); err != nil {
			_ = rows.Close()
			_ = s.invalidateProjectLocked(ctx, projectID)
			return nil, &Error{Code: ECorrupt, BranchID: projectID, Field: "execution_state_json", Cause: err}
		}
		if err := json.Unmarshal([]byte(w.BranchResultsJSON), &w.BranchResults); err != nil {
			_ = rows.Close()
			_ = s.invalidateProjectLocked(ctx, projectID)
			return nil, &Error{Code: ECorrupt, BranchID: projectID, Field: "branch_results_json", Cause: err}
		}
		if err := json.Unmarshal([]byte(w.ConditionSnapshotsJSON), &w.ConditionSnapshots); err != nil {
			_ = rows.Close()
			_ = s.invalidateProjectLocked(ctx, projectID)
			return nil, &Error{Code: ECorrupt, BranchID: projectID, Field: "condition_snapshots_json", Cause: err}
		}
		windows = append(windows, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(windows) == 0 {
		return nil, nil
	}
	last := windows[len(windows)-1]
	return &CheckpointSnapshot{ProjectID: projectID, FormatNo: last.FormatNo, FormatVersion: last.FormatVersion, Windows: windows, State: last.State}, nil
}

func (s *SQLiteStore) InvalidateProject(ctx context.Context, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateProjectLocked(ctx, projectID)
}

func (s *SQLiteStore) invalidateProjectLocked(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM frame_windows WHERE project_id=?`, projectID)
	return err
}

func (s *SQLiteStore) CorruptWindowHash(ctx context.Context, projectID string, windowIndex int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE frame_windows SET content_hash='injected-bad-hash' WHERE project_id=? AND window_index=?`, projectID, windowIndex)
	return err
}

func (s *SQLiteStore) CorruptWindowVersion(ctx context.Context, projectID string, windowIndex int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE frame_windows SET format_version='0.0-injected' WHERE project_id=? AND window_index=?`, projectID, windowIndex)
	return err
}

func (s *SQLiteStore) CountWindows(ctx context.Context, projectID string, status string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT COUNT(*) FROM frame_windows WHERE project_id=?`
	args := []any{projectID}
	if status != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *SQLiteStore) SaveBranchCache(projectID string, entry CacheEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := CanonicalJSON(entry.State)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
INSERT INTO result_cache(project_id,branch_id,content_hash,result_json)
VALUES(?,?,?,?)
ON CONFLICT(project_id,branch_id) DO UPDATE SET content_hash=excluded.content_hash,result_json=excluded.result_json`,
		projectID, entry.BranchID, entry.ContentHash, string(data))
	return err
}

func (s *SQLiteStore) LoadBranchCache(projectID string) ([]CacheEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT branch_id,content_hash,result_json FROM result_cache WHERE project_id=? ORDER BY branch_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CacheEntry{}
	for rows.Next() {
		var entry CacheEntry
		var raw string
		if err := rows.Scan(&entry.BranchID, &entry.ContentHash, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &entry.State); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteBranchCache(projectID, branchID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM result_cache WHERE project_id=? AND branch_id=?`, projectID, branchID)
	return err
}
