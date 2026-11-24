package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"roomworkbench/internal/wb"
)

type SQLiteStore struct {
	db   *sql.DB
	mu   sync.Mutex
	path string
}

func Open(ctx context.Context, path string) (*SQLiteStore, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &SQLiteStore{db: db, path: path}
	if err := s.initSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) initSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			key TEXT PRIMARY KEY,
			value BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS room_checkpoints (
			room_id TEXT PRIMARY KEY,
			commit_order INTEGER NOT NULL UNIQUE,
			input_hash TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			param_version INTEGER NOT NULL,
			algo_version INTEGER NOT NULL,
			params_json BLOB NOT NULL,
			openings_json BLOB NOT NULL,
			paths_json BLOB NOT NULL,
			listeners_json BLOB NOT NULL,
			committed_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("init sqlite schema: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

// SaveCheckpoint serializes every writer and commits meta + room row in one
// SQLite transaction. Writes from goroutines therefore cannot interleave.
func (s *SQLiteStore) SaveCheckpoint(ctx context.Context, item wb.StoredCheckpoint, sampleJSON []byte, revision int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	meta := []struct {
		key   string
		value []byte
	}{
		{"input_hash", []byte(item.InputHash)},
		{"revision", []byte(fmt.Sprintf("%d", revision))},
		{"param_version", []byte(fmt.Sprintf("%d", item.ParamVersion))},
		{"algo_version", []byte(fmt.Sprintf("%d", item.AlgorithmVersion))},
	}
	if sampleJSON != nil {
		meta = append(meta, struct {
			key   string
			value []byte
		}{"sample_json", sampleJSON})
	}
	for _, m := range meta {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO checkpoint_meta(key,value) VALUES(?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			m.key, m.value); err != nil {
			return err
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO room_checkpoints(
			room_id, commit_order, input_hash, content_hash,
			param_version, algo_version,
			params_json, openings_json, paths_json, listeners_json)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(room_id) DO UPDATE SET
			commit_order=excluded.commit_order,
			input_hash=excluded.input_hash,
			content_hash=excluded.content_hash,
			param_version=excluded.param_version,
			algo_version=excluded.algo_version,
			params_json=excluded.params_json,
			openings_json=excluded.openings_json,
			paths_json=excluded.paths_json,
			listeners_json=excluded.listeners_json,
			committed_at=datetime('now')`,
		item.RoomID, item.CommitOrder, item.InputHash, item.ContentHash,
		item.ParamVersion, item.AlgorithmVersion,
		item.ParamsJSON, item.OpeningsJSON, item.PathsJSON, item.ListenersJSON)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) LoadCheckpoints(ctx context.Context) (wb.CheckpointBundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bundle := wb.CheckpointBundle{}
	if value, ok, err := loadMeta(ctx, s.db, "input_hash"); err != nil {
		return bundle, err
	} else if ok {
		bundle.InputHash = string(value)
	}
	if value, ok, err := loadMeta(ctx, s.db, "revision"); err != nil {
		return bundle, err
	} else if ok {
		_, _ = fmt.Sscanf(string(value), "%d", &bundle.Revision)
	}
	if value, ok, err := loadMeta(ctx, s.db, "sample_json"); err != nil {
		return bundle, err
	} else if ok {
		bundle.SampleJSON = value
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT room_id, commit_order, input_hash, content_hash,
		       param_version, algo_version,
		       params_json, openings_json, paths_json, listeners_json
		FROM room_checkpoints
		ORDER BY commit_order, room_id`)
	if err != nil {
		return bundle, err
	}
	defer rows.Close()

	for rows.Next() {
		var item wb.StoredCheckpoint
		if err := rows.Scan(
			&item.RoomID, &item.CommitOrder, &item.InputHash, &item.ContentHash,
			&item.ParamVersion, &item.AlgorithmVersion,
			&item.ParamsJSON, &item.OpeningsJSON, &item.PathsJSON, &item.ListenersJSON); err != nil {
			return bundle, err
		}
		bundle.Items = append(bundle.Items, item)
	}
	return bundle, rows.Err()
}

func loadMeta(ctx context.Context, db *sql.DB, key string) ([]byte, bool, error) {
	var value []byte
	err := db.QueryRowContext(ctx, `SELECT value FROM checkpoint_meta WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (s *SQLiteStore) ResetCheckpoints(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM room_checkpoints`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_meta`); err != nil {
		return err
	}
	return tx.Commit()
}
