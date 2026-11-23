package stage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

type CheckpointStore struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

type CheckpointLoad struct {
	Windows     []WindowResult `json:"windows"`
	Invalidated bool           `json:"invalidated"`
	Reason      string         `json:"reason,omitempty"`
}

type checkpointMeta struct {
	ProjectID        string
	ContentHash      string
	ParamVersion     string
	AlgorithmVersion string
	TotalWindows     int
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	if path == "" {
		return nil, newError(CodeStorage, "checkpoint path is required", "checkpoint", "path")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, storageError("open checkpoint", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &CheckpointStore{db: db, path: path}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) initialize() error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			project_id TEXT PRIMARY KEY,
			content_hash TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			total_windows INTEGER NOT NULL,
			updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_windows (
			project_id TEXT NOT NULL,
			window_index INTEGER NOT NULL,
			payload TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (project_id, window_index),
			FOREIGN KEY (project_id) REFERENCES checkpoint_meta(project_id) ON DELETE CASCADE
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return storageError("initialize checkpoint", err)
		}
	}
	return nil
}

func (s *CheckpointStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *CheckpointStore) Load(projectID, contentHash, paramVersion, algorithmVersion string, totalWindows int) (CheckpointLoad, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return CheckpointLoad{}, newError(CodeStorage, "checkpoint is closed", "checkpoint", "db")
	}
	meta, found, err := s.readMeta(projectID)
	if err != nil {
		return CheckpointLoad{}, err
	}
	if !found {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM checkpoint_windows WHERE project_id = ?`, projectID).Scan(&count); err != nil {
			return CheckpointLoad{}, storageError("inspect checkpoint", err)
		}
		if count == 0 {
			return CheckpointLoad{}, nil
		}
		if err := s.resetLocked(projectID); err != nil {
			return CheckpointLoad{}, err
		}
		return CheckpointLoad{Invalidated: true, Reason: "checkpoint metadata is missing"}, nil
	}
	if meta.ContentHash != contentHash {
		if err := s.resetLocked(projectID); err != nil {
			return CheckpointLoad{}, err
		}
		return CheckpointLoad{Invalidated: true, Reason: "content hash changed; entire checkpoint discarded"}, nil
	}
	if meta.ParamVersion != paramVersion {
		if err := s.resetLocked(projectID); err != nil {
			return CheckpointLoad{}, err
		}
		return CheckpointLoad{Invalidated: true, Reason: "parameter version changed; entire checkpoint discarded"}, nil
	}
	if meta.AlgorithmVersion != algorithmVersion {
		if err := s.resetLocked(projectID); err != nil {
			return CheckpointLoad{}, err
		}
		return CheckpointLoad{Invalidated: true, Reason: "algorithm version changed; entire checkpoint discarded"}, nil
	}
	if meta.TotalWindows != totalWindows {
		if err := s.resetLocked(projectID); err != nil {
			return CheckpointLoad{}, err
		}
		return CheckpointLoad{Invalidated: true, Reason: "window count changed; entire checkpoint discarded"}, nil
	}
	rows, err := s.db.Query(`SELECT window_index, payload FROM checkpoint_windows WHERE project_id = ? ORDER BY window_index ASC`, projectID)
	if err != nil {
		return CheckpointLoad{}, storageError("query checkpoint windows", err)
	}
	defer rows.Close()
	var windows []WindowResult
	expectedIndex := 0
	for rows.Next() {
		var index int
		var payload string
		if err := rows.Scan(&index, &payload); err != nil {
			return CheckpointLoad{}, storageError("scan checkpoint window", err)
		}
		if index != expectedIndex {
			if err := rows.Close(); err != nil {
				return CheckpointLoad{}, storageError("close checkpoint rows", err)
			}
			if err := s.resetLocked(projectID); err != nil {
				return CheckpointLoad{}, err
			}
			return CheckpointLoad{Invalidated: true, Reason: "checkpoint windows are not contiguous"}, nil
		}
		var window WindowResult
		if err := json.Unmarshal([]byte(payload), &window); err != nil {
			_ = rows.Close()
			if err := s.resetLocked(projectID); err != nil {
				return CheckpointLoad{}, err
			}
			return CheckpointLoad{Invalidated: true, Reason: "checkpoint payload is corrupt"}, nil
		}
		if window.ContentHash != contentHash || window.ParamVersion != paramVersion || window.AlgorithmVersion != algorithmVersion {
			_ = rows.Close()
			if err := s.resetLocked(projectID); err != nil {
				return CheckpointLoad{}, err
			}
			return CheckpointLoad{Invalidated: true, Reason: "window version marker changed; entire checkpoint discarded"}, nil
		}
		window.State = WindowCommitted
		windows = append(windows, window)
		expectedIndex++
	}
	if err := rows.Err(); err != nil {
		return CheckpointLoad{}, storageError("iterate checkpoint windows", err)
	}
	if len(windows) > totalWindows {
		_ = rows.Close()
		if err := s.resetLocked(projectID); err != nil {
			return CheckpointLoad{}, err
		}
		return CheckpointLoad{Invalidated: true, Reason: "checkpoint contains more windows than the project"}, nil
	}
	return CheckpointLoad{Windows: windows}, nil
}

func (s *CheckpointStore) SaveWindow(projectID string, result WindowResult, totalWindows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return newError(CodeStorage, "checkpoint is closed", "checkpoint", "db")
	}
	result.State = WindowCommitted
	payload, err := json.Marshal(result)
	if err != nil {
		return storageError("marshal checkpoint window", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return storageError("begin checkpoint transaction", err)
	}
	defer func() { _ = tx.Rollback() }()
	meta, found, err := readMetaTx(tx, projectID)
	if err != nil {
		return err
	}
	if !found {
		if _, err := tx.Exec(
			`INSERT INTO checkpoint_meta(project_id, content_hash, param_version, algorithm_version, total_windows) VALUES(?,?,?,?,?)`,
			projectID, result.ContentHash, result.ParamVersion, result.AlgorithmVersion, totalWindows,
		); err != nil {
			return storageError("insert checkpoint metadata", err)
		}
	} else {
		if meta.ContentHash != result.ContentHash || meta.ParamVersion != result.ParamVersion ||
			meta.AlgorithmVersion != result.AlgorithmVersion || meta.TotalWindows != totalWindows {
			err := newError(CodeState, "checkpoint version changed during commit", "checkpoint", "version")
			return withDetail(err, ErrorDetail{Window: result.Index, Reason: "all windows must share one content/parameter/algorithm version"})
		}
	}
	var lastIndex int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(window_index), -1) FROM checkpoint_windows WHERE project_id = ?`, projectID).Scan(&lastIndex); err != nil {
		return storageError("read checkpoint commit offset", err)
	}
	if result.Index != lastIndex+1 {
		err := newError(CodeState, "checkpoint windows must commit in contiguous order", "checkpoint", "window_index")
		return withDetail(err, ErrorDetail{Expected: fmt.Sprint(lastIndex + 1), Actual: fmt.Sprint(result.Index), Window: result.Index})
	}
	if _, err := tx.Exec(
		`INSERT INTO checkpoint_windows(project_id, window_index, payload) VALUES(?,?,?)`,
		projectID, result.Index, string(payload),
	); err != nil {
		return storageError("insert checkpoint window", err)
	}
	if _, err := tx.Exec(`UPDATE checkpoint_meta SET updated_at = CURRENT_TIMESTAMP WHERE project_id = ?`, projectID); err != nil {
		return storageError("touch checkpoint metadata", err)
	}
	if err := tx.Commit(); err != nil {
		return storageError("commit checkpoint transaction", err)
	}
	return nil
}

func (s *CheckpointStore) Reset(projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return newError(CodeStorage, "checkpoint is closed", "checkpoint", "db")
	}
	return s.resetLocked(projectID)
}

// RebaseProject explicitly advances metadata for committed windows whose
// outputs are known to be unaffected by an edit. Frame payload values are not
// changed; only the content/parameter/algorithm markers are rewritten in one
// serialized transaction. Callers must reject edits that touch committed time.
func (s *CheckpointStore) RebaseProject(projectID, oldContentHash, newContentHash, oldParamVersion, newParamVersion, algorithmVersion string, totalWindows int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0, newError(CodeStorage, "checkpoint is closed", "checkpoint", "db")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, storageError("begin checkpoint rebase", err)
	}
	defer func() { _ = tx.Rollback() }()
	meta, found, err := readMetaTx(tx, projectID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, newError(CodeState, "cannot rebase a missing checkpoint", "checkpoint", "metadata")
	}
	if meta.ContentHash != oldContentHash || meta.ParamVersion != oldParamVersion || meta.AlgorithmVersion != algorithmVersion || meta.TotalWindows != totalWindows {
		return 0, newError(CodeState, "checkpoint version does not match rebase source", "checkpoint", "version")
	}
	rows, err := tx.Query(`SELECT window_index, payload FROM checkpoint_windows WHERE project_id = ? ORDER BY window_index ASC`, projectID)
	if err != nil {
		return 0, storageError("query rebase windows", err)
	}
	type rowPayload struct {
		index   int
		payload string
	}
	var items []rowPayload
	for rows.Next() {
		var item rowPayload
		if err := rows.Scan(&item.index, &item.payload); err != nil {
			rows.Close()
			return 0, storageError("scan rebase window", err)
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, storageError("close rebase windows", err)
	}
	for _, item := range items {
		var window WindowResult
		if err := json.Unmarshal([]byte(item.payload), &window); err != nil {
			return 0, storageError("decode rebase window", err)
		}
		if window.ContentHash != oldContentHash || window.ParamVersion != oldParamVersion || window.AlgorithmVersion != algorithmVersion {
			return 0, newError(CodeState, "window version does not match rebase source", fmt.Sprintf("window:%d", item.index), "version")
		}
		window.ContentHash = newContentHash
		window.ParamVersion = newParamVersion
		window.AlgorithmVersion = algorithmVersion
		payload, err := json.Marshal(window)
		if err != nil {
			return 0, storageError("encode rebase window", err)
		}
		if _, err := tx.Exec(`UPDATE checkpoint_windows SET payload = ? WHERE project_id = ? AND window_index = ?`, string(payload), projectID, item.index); err != nil {
			return 0, storageError("update rebase window", err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE checkpoint_meta SET content_hash = ?, param_version = ?, algorithm_version = ?, total_windows = ?, updated_at = CURRENT_TIMESTAMP WHERE project_id = ?`,
		newContentHash, newParamVersion, algorithmVersion, totalWindows, projectID,
	); err != nil {
		return 0, storageError("update rebase metadata", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, storageError("commit checkpoint rebase", err)
	}
	return len(items), nil
}

func (s *CheckpointStore) resetLocked(projectID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return storageError("begin checkpoint reset", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM checkpoint_windows WHERE project_id = ?`, projectID); err != nil {
		return storageError("delete checkpoint windows", err)
	}
	if _, err := tx.Exec(`DELETE FROM checkpoint_meta WHERE project_id = ?`, projectID); err != nil {
		return storageError("delete checkpoint metadata", err)
	}
	if err := tx.Commit(); err != nil {
		return storageError("commit checkpoint reset", err)
	}
	return nil
}

func (s *CheckpointStore) readMeta(projectID string) (checkpointMeta, bool, error) {
	row := s.db.QueryRow(
		`SELECT content_hash, param_version, algorithm_version, total_windows FROM checkpoint_meta WHERE project_id = ?`,
		projectID,
	)
	var meta checkpointMeta
	err := row.Scan(&meta.ContentHash, &meta.ParamVersion, &meta.AlgorithmVersion, &meta.TotalWindows)
	if err == sql.ErrNoRows {
		return checkpointMeta{}, false, nil
	}
	if err != nil {
		return checkpointMeta{}, false, storageError("read checkpoint metadata", err)
	}
	meta.ProjectID = projectID
	return meta, true, nil
}

func readMetaTx(tx *sql.Tx, projectID string) (checkpointMeta, bool, error) {
	row := tx.QueryRow(
		`SELECT content_hash, param_version, algorithm_version, total_windows FROM checkpoint_meta WHERE project_id = ?`,
		projectID,
	)
	var meta checkpointMeta
	err := row.Scan(&meta.ContentHash, &meta.ParamVersion, &meta.AlgorithmVersion, &meta.TotalWindows)
	if err == sql.ErrNoRows {
		return checkpointMeta{}, false, nil
	}
	if err != nil {
		return checkpointMeta{}, false, storageError("read checkpoint metadata", err)
	}
	meta.ProjectID = projectID
	return meta, true, nil
}

func storageError(action string, cause error) *DomainError {
	err := newError(CodeStorage, action, "checkpoint", "")
	err.Cause = cause.Error()
	return err
}

func (s *CheckpointStore) Path() string { return s.path }
