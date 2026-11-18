package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"configcenter/internal/model"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS namespaces (
	name TEXT PRIMARY KEY,
	current_version INTEGER NOT NULL CHECK (current_version >= 0),
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS config_items (
	namespace TEXT NOT NULL,
	item_key TEXT NOT NULL,
	value TEXT NOT NULL,
	version INTEGER NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (namespace, item_key),
	FOREIGN KEY (namespace) REFERENCES namespaces(name) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS config_history (
	namespace TEXT NOT NULL,
	item_key TEXT NOT NULL,
	value TEXT NOT NULL,
	version INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (namespace, item_key, version),
	FOREIGN KEY (namespace) REFERENCES namespaces(name) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_history_namespace_version
	ON config_history(namespace, version);
`

type Store struct {
	db        *sql.DB
	publishMu sync.Mutex
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("database path is required")
	}
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	var version string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("read sqlite version: %w", err)
	}
	if err := checkSQLiteVersion(version); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) SQLiteVersion(ctx context.Context) (string, error) {
	var version string
	if err := s.db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}

func checkSQLiteVersion(version string) error {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return fmt.Errorf("unrecognized sqlite version %q", version)
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return fmt.Errorf("unrecognized sqlite version %q", version)
	}
	if major < 3 || (major == 3 && minor < 40) {
		return fmt.Errorf("sqlite version %s is too old; 3.40 or newer is required", version)
	}
	return nil
}

func (s *Store) EnsureNamespace(ctx context.Context, namespace string, defaults map[string]model.Change) (int64, bool, error) {
	if strings.TrimSpace(namespace) == "" {
		return 0, false, model.NewAPIError("E_VALIDATION", "namespace is required", model.ErrValidation.HTTPStatus)
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var current int64
	err = tx.QueryRowContext(ctx, "SELECT current_version FROM namespaces WHERE name = ?", namespace).Scan(&current)
	if err == nil {
		return current, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	initialVersion := int64(0)
	if len(defaults) > 0 {
		initialVersion = 1
	}
	if _, err := tx.ExecContext(
		ctx,
		"INSERT INTO namespaces(name, current_version, created_at, updated_at) VALUES (?, ?, ?, ?)",
		namespace, initialVersion, now, now,
	); err != nil {
		return 0, false, err
	}

	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		change := defaults[key]
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO config_items(namespace, item_key, value, version, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			namespace, key, change.Value, initialVersion, now,
		); err != nil {
			return 0, false, err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO config_history(namespace, item_key, value, version, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			namespace, key, change.Value, initialVersion, now,
		); err != nil {
			return 0, false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return initialVersion, true, nil
}

func (s *Store) Publish(ctx context.Context, namespace string, changes []model.Change) (int64, error) {
	if err := ValidateChanges(changes); err != nil {
		return 0, err
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var current int64
	err = tx.QueryRowContext(ctx, "SELECT current_version FROM namespaces WHERE name = ?", namespace).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, model.ErrNamespaceNotFound
	}
	if err != nil {
		return 0, err
	}

	newVersion := current + 1
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, change := range changes {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO config_items(namespace, item_key, value, version, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(namespace, item_key) DO UPDATE SET
			   value = excluded.value,
			   version = excluded.version,
			   updated_at = excluded.updated_at`,
			namespace, change.Key, change.Value, newVersion, now,
		); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO config_history(namespace, item_key, value, version, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			namespace, change.Key, change.Value, newVersion, now,
		); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		"UPDATE namespaces SET current_version = ?, updated_at = ? WHERE name = ?",
		newVersion, now, namespace,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newVersion, nil
}

func (s *Store) LatestVersion(ctx context.Context, namespace string) (int64, error) {
	var version int64
	err := s.db.QueryRowContext(ctx, "SELECT current_version FROM namespaces WHERE name = ?", namespace).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, model.ErrNamespaceNotFound
	}
	return version, err
}

func (s *Store) ListNamespaces(ctx context.Context) ([]model.CurrentResponse, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, current_version FROM namespaces ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]model.CurrentResponse, 0)
	for rows.Next() {
		var item model.CurrentResponse
		if err := rows.Scan(&item.Namespace, &item.Version); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ChangesSince(ctx context.Context, namespace string, fromVersion, toVersion int64) ([]model.Item, error) {
	if fromVersion < 0 || toVersion < fromVersion {
		return nil, model.NewAPIError("E_BAD_REQUEST", "invalid version range", model.ErrBadRequest.HTTPStatus)
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT h.item_key, h.value, h.version
		 FROM config_history h
		 JOIN (
		   SELECT item_key, MAX(version) AS max_version
		   FROM config_history
		   WHERE namespace = ? AND version > ? AND version <= ?
		   GROUP BY item_key
		 ) latest ON latest.item_key = h.item_key AND latest.max_version = h.version
		 WHERE h.namespace = ?
		 ORDER BY h.version ASC, h.item_key ASC`,
		namespace, fromVersion, toVersion, namespace,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]model.Item, 0)
	for rows.Next() {
		var item model.Item
		if err := rows.Scan(&item.Key, &item.Value, &item.Version); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) SnapshotAt(ctx context.Context, namespace string, version int64) (map[string]string, map[string]int64, error) {
	if version < 1 {
		return nil, nil, model.ErrVersionNotFound
	}
	latest, err := s.LatestVersion(ctx, namespace)
	if err != nil {
		return nil, nil, err
	}
	if version > latest {
		return nil, nil, model.ErrVersionNotFound
	}

	rows, err := s.db.QueryContext(
		ctx,
		`SELECT h.item_key, h.value, h.version
		 FROM config_history h
		 JOIN (
		   SELECT item_key, MAX(version) AS max_version
		   FROM config_history
		   WHERE namespace = ? AND version <= ?
		   GROUP BY item_key
		 ) latest ON latest.item_key = h.item_key AND latest.max_version = h.version
		 WHERE h.namespace = ?
		 ORDER BY h.item_key ASC`,
		namespace, version, namespace,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	config := make(map[string]string)
	versions := make(map[string]int64)
	for rows.Next() {
		var key, value string
		var itemVersion int64
		if err := rows.Scan(&key, &value, &itemVersion); err != nil {
			return nil, nil, err
		}
		config[key] = value
		versions[key] = itemVersion
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return config, versions, nil
}

func (s *Store) CurrentSnapshot(ctx context.Context, namespace string) (int64, map[string]string, error) {
	version, err := s.LatestVersion(ctx, namespace)
	if err != nil {
		return 0, nil, err
	}
	if version == 0 {
		return 0, map[string]string{}, nil
	}
	config, _, err := s.SnapshotAt(ctx, namespace, version)
	if err != nil {
		return 0, nil, err
	}
	return version, config, nil
}
