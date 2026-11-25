package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"chaosdiag/model"

	_ "modernc.org/sqlite"
)

type Checkpoint struct {
	ProjectVersion string                `json:"project_version"`
	LastTick       int                   `json:"last_tick"`
	Requests       []model.RequestResult `json:"requests"`
	Queue          []model.QueueItem     `json:"queue"`
}

type Store struct {
	db   *sql.DB
	path string
}

func OpenStore(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, model.NewError(model.CodeInternal, "checkpoint.path", "create checkpoint directory: %v", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, corruptError(path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, corruptError(path, err)
	}
	store := &Store{db: db, path: path}
	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) initSchema() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS request_states (
			request_id TEXT PRIMARY KEY,
			tick INTEGER NOT NULL,
			status TEXT NOT NULL,
			completed_hops INTEGER NOT NULL,
			result_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS wait_queue (
			instruction_id TEXT PRIMARY KEY,
			queue_position INTEGER NOT NULL,
			since_tick INTEGER NOT NULL,
			item_json TEXT NOT NULL
		)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return corruptError(s.path, err)
		}
	}
	return nil
}

func (s *Store) Save(cp Checkpoint) error {
	tx, err := s.db.Begin()
	if err != nil {
		return corruptError(s.path, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM request_states`); err != nil {
		return corruptError(s.path, err)
	}
	for _, rr := range cp.Requests {
		raw, err := json.Marshal(rr)
		if err != nil {
			return model.NewError(model.CodeInternal, "request_states.result_json", "encode request %s: %v", rr.RequestID, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO request_states(request_id,tick,status,completed_hops,result_json) VALUES(?,?,?,?,?)`,
			rr.RequestID, rr.Tick, rr.Status, rr.CompletedHops, string(raw),
		); err != nil {
			return corruptError(s.path, err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM wait_queue`); err != nil {
		return corruptError(s.path, err)
	}
	for _, item := range cp.Queue {
		raw, err := json.Marshal(item)
		if err != nil {
			return model.NewError(model.CodeInternal, "wait_queue.item_json", "encode queue item %s: %v", item.InstructionID, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO wait_queue(instruction_id,queue_position,since_tick,item_json) VALUES(?,?,?,?)`,
			item.InstructionID, item.Position, item.SinceTick, string(raw),
		); err != nil {
			return corruptError(s.path, err)
		}
	}

	if _, err := tx.Exec(
		`INSERT INTO checkpoint_meta(key,value) VALUES('project_version',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		cp.ProjectVersion,
	); err != nil {
		return corruptError(s.path, err)
	}

	// Global logical tick is intentionally written last. The transaction commit
	// is the only visibility boundary: readers either see the complete tick or
	// the previous committed checkpoint.
	if _, err := tx.Exec(
		`INSERT INTO checkpoint_meta(key,value) VALUES('last_tick',?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		intToText(cp.LastTick),
	); err != nil {
		return corruptError(s.path, err)
	}

	if err := tx.Commit(); err != nil {
		return corruptError(s.path, err)
	}
	return nil
}

func Load(path string) (*Checkpoint, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, model.NewError(model.CodeCheckpointCorrupt, "checkpoint.path", "stat failed: %v", err)
	}
	store, err := OpenStore(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.Load()
}

func LoadExpected(path, expectedVersion string) (*Checkpoint, error) {
	cp, err := Load(path)
	if err != nil {
		return nil, err
	}
	if cp == nil {
		return nil, model.NewError(model.CodeCheckpointCorrupt, "checkpoint.path", "checkpoint does not exist")
	}
	if cp.ProjectVersion != expectedVersion {
		ce := model.NewError(model.CodeCheckpointVersionMismatch, "checkpoint.project_version",
			"checkpoint version %q does not match project version %q; full replay required", cp.ProjectVersion, expectedVersion)
		model.WithContext(ce, "checkpoint_version", cp.ProjectVersion)
		model.WithContext(ce, "expected_version", expectedVersion)
		return nil, ce
	}
	return cp, nil
}

func (s *Store) Load() (*Checkpoint, error) {
	cp := &Checkpoint{}
	var lastTick string
	err := s.db.QueryRow(`SELECT value FROM checkpoint_meta WHERE key='last_tick'`).Scan(&lastTick)
	if err != nil {
		return nil, corruptError(s.path, err)
	}
	cp.LastTick = textToInt(lastTick)

	var version string
	if err := s.db.QueryRow(`SELECT value FROM checkpoint_meta WHERE key='project_version'`).Scan(&version); err != nil {
		return nil, corruptError(s.path, err)
	}
	cp.ProjectVersion = version

	rows, err := s.db.Query(`SELECT result_json FROM request_states ORDER BY request_id`)
	if err != nil {
		return nil, corruptError(s.path, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, corruptError(s.path, err)
		}
		var rr model.RequestResult
		if err := json.Unmarshal([]byte(raw), &rr); err != nil {
			return nil, corruptError(s.path, err)
		}
		cp.Requests = append(cp.Requests, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, corruptError(s.path, err)
	}

	qrows, err := s.db.Query(`SELECT item_json FROM wait_queue ORDER BY queue_position`)
	if err != nil {
		return nil, corruptError(s.path, err)
	}
	defer qrows.Close()
	for qrows.Next() {
		var raw string
		if err := qrows.Scan(&raw); err != nil {
			return nil, corruptError(s.path, err)
		}
		var item model.QueueItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return nil, corruptError(s.path, err)
		}
		cp.Queue = append(cp.Queue, item)
	}
	if err := qrows.Err(); err != nil {
		return nil, corruptError(s.path, err)
	}
	return cp, nil
}

func corruptError(path string, cause error) *model.Error {
	ce := model.NewError(model.CodeCheckpointCorrupt, "checkpoint.file", "unreadable SQLite checkpoint %s: %v", path, cause)
	model.WithContext(ce, "path", path)
	return ce
}

func intToText(v int) string { return strconv.Itoa(v) }

func textToInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
