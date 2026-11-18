package clientstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"example.com/dualring-config/internal/model"
)

type Snapshot struct {
	InstanceID string
	Namespace  string
	Version    int64
	Ring       string
	ReleaseID  string
	Digest     string
	Items      []model.Item
	LoadedAt   string
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("local snapshot database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create snapshot directory: %w", err)
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=cache_size(-2000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS client_snapshots (
			instance_id TEXT NOT NULL,
			namespace TEXT NOT NULL,
			version INTEGER NOT NULL,
			ring TEXT NOT NULL,
			release_id TEXT NOT NULL DEFAULT '',
			digest TEXT NOT NULL,
			items_json TEXT NOT NULL,
			loaded_at TEXT NOT NULL,
			PRIMARY KEY(instance_id, namespace)
		)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Save(ctx context.Context, snapshot Snapshot) error {
	itemsJSON, err := model.MarshalItems(snapshot.Items)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO client_snapshots
		 (instance_id, namespace, version, ring, release_id, digest, items_json, loaded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(instance_id, namespace) DO UPDATE SET
		   version = excluded.version,
		   ring = excluded.ring,
		   release_id = excluded.release_id,
		   digest = excluded.digest,
		   items_json = excluded.items_json,
		   loaded_at = excluded.loaded_at`,
		snapshot.InstanceID, snapshot.Namespace, snapshot.Version, snapshot.Ring,
		snapshot.ReleaseID, snapshot.Digest, itemsJSON, snapshot.LoadedAt,
	)
	return err
}

func (s *Store) Load(ctx context.Context, instanceID, namespace string) (Snapshot, bool, error) {
	var snapshot Snapshot
	var itemsJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT instance_id, namespace, version, ring, release_id, digest, items_json, loaded_at
		 FROM client_snapshots WHERE instance_id = ? AND namespace = ?`,
		instanceID, namespace,
	).Scan(
		&snapshot.InstanceID, &snapshot.Namespace, &snapshot.Version, &snapshot.Ring,
		&snapshot.ReleaseID, &snapshot.Digest, &itemsJSON, &snapshot.LoadedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	snapshot.Items, err = model.UnmarshalItems(itemsJSON)
	if err != nil {
		return Snapshot{}, false, err
	}
	return snapshot, true, nil
}
