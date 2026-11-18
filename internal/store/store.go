package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"example.com/dualring-config/internal/model"
)

const (
	defaultHistoryLimit = 20
	defaultThreshold    = 0.005
)

type Options struct {
	CanaryPercent     float64
	ErrorThreshold    float64
	HistoryLimit      int
	ObservationPeriod time.Duration
	Now               func() time.Time
}

type Store struct {
	db                *sql.DB
	writeMu           sync.Mutex
	canaryPercent     float64
	errorThreshold    float64
	historyLimit      int
	observationPeriod time.Duration
	now               func() time.Time
}

func Open(path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	dsn := sqliteDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	s := &Store{
		db:                db,
		canaryPercent:     opts.CanaryPercent,
		errorThreshold:    opts.ErrorThreshold,
		historyLimit:      opts.HistoryLimit,
		observationPeriod: opts.ObservationPeriod,
		now:               opts.Now,
	}
	if s.canaryPercent < 0 || s.canaryPercent > 100 {
		_ = db.Close()
		return nil, errors.New("canary percent must be between 0 and 100")
	}
	if s.errorThreshold <= 0 {
		s.errorThreshold = defaultThreshold
	}
	if s.historyLimit < 5 {
		s.historyLimit = defaultHistoryLimit
	}
	if s.observationPeriod <= 0 {
		s.observationPeriod = 10 * time.Second
	}
	if s.now == nil {
		s.now = time.Now
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func sqliteDSN(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_pragma=cache_size(-4000)"
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Health(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (s *Store) ErrorThreshold() float64 {
	return s.errorThreshold
}

func (s *Store) nowText() string {
	return s.now().UTC().Format(time.RFC3339Nano)
}

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS config_items (
			namespace TEXT NOT NULL,
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			digest TEXT NOT NULL,
			item_version INTEGER NOT NULL,
			updated_at TEXT NOT NULL,
			updated_by TEXT NOT NULL,
			PRIMARY KEY (namespace, key)
		)`,
		`CREATE TABLE IF NOT EXISTS config_item_versions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace TEXT NOT NULL,
			key TEXT NOT NULL,
			version INTEGER NOT NULL,
			value TEXT NOT NULL,
			digest TEXT NOT NULL,
			action TEXT NOT NULL,
			operator TEXT NOT NULL,
			release_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			UNIQUE(namespace, key, version)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_item_versions_lookup
			ON config_item_versions(namespace, key, version DESC)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace TEXT NOT NULL,
			kind TEXT NOT NULL,
			release_id TEXT NOT NULL DEFAULT '',
			digest TEXT NOT NULL,
			operator TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_namespace ON snapshots(namespace, id DESC)`,
		`CREATE TABLE IF NOT EXISTS snapshot_items (
			snapshot_id INTEGER NOT NULL,
			namespace TEXT NOT NULL,
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			digest TEXT NOT NULL,
			item_version INTEGER NOT NULL,
			PRIMARY KEY (snapshot_id, key),
			FOREIGN KEY(snapshot_id) REFERENCES snapshots(id)
		)`,
		`CREATE TABLE IF NOT EXISTS namespace_heads (
			namespace TEXT PRIMARY KEY,
			snapshot_id INTEGER NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY(snapshot_id) REFERENCES snapshots(id)
		)`,
		`CREATE TABLE IF NOT EXISTS releases (
			id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			state TEXT NOT NULL,
			operator TEXT NOT NULL,
			base_snapshot_id INTEGER NOT NULL,
			candidate_snapshot_id INTEGER NOT NULL,
			observation_seconds REAL NOT NULL,
			created_at TEXT NOT NULL,
			canary_started_at TEXT NOT NULL DEFAULT '',
			observation_ends_at TEXT NOT NULL DEFAULT '',
			full_at TEXT NOT NULL DEFAULT '',
			passed_at TEXT NOT NULL DEFAULT '',
			rolled_back_at TEXT NOT NULL DEFAULT '',
			rollback_reason TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_one_active_release
			ON releases(namespace)
			WHERE state IN ('draft', 'canary', 'observed_pass')`,
		`CREATE INDEX IF NOT EXISTS idx_releases_namespace ON releases(namespace, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS release_reports (
			release_id TEXT NOT NULL,
			instance_id TEXT NOT NULL,
			success_count INTEGER NOT NULL,
			error_count INTEGER NOT NULL,
			local_version INTEGER NOT NULL,
			offline INTEGER NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (release_id, instance_id)
		)`,
		`CREATE TABLE IF NOT EXISTS clients (
			instance_id TEXT PRIMARY KEY,
			ring TEXT NOT NULL,
			bucket INTEGER NOT NULL,
			assigned_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_clients_ring ON clients(ring)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			operation TEXT NOT NULL,
			namespace TEXT NOT NULL DEFAULT '',
			key TEXT NOT NULL DEFAULT '',
			release_id TEXT NOT NULL DEFAULT '',
			old_digest TEXT NOT NULL DEFAULT '',
			new_digest TEXT NOT NULL DEFAULT '',
			operator TEXT NOT NULL,
			status_code TEXT NOT NULL,
			details TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_namespace ON audit_log(namespace, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_release ON audit_log(release_id, id DESC)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES('schema_version', '1')
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	if err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return nil
}

func (s *Store) insertAuditTx(
	ctx context.Context,
	tx *sql.Tx,
	operation, namespace, key, releaseID, oldDigest, newDigest, operator, status, details string,
) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log
		 (created_at, operation, namespace, key, release_id, old_digest, new_digest, operator, status_code, details)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.nowText(), operation, namespace, key, releaseID, oldDigest, newDigest, operator, status, details,
	)
	return err
}

func (s *Store) recordFailureAudit(
	operation, namespace, key, releaseID, oldDigest, newDigest, operator, status, details string,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log
		 (created_at, operation, namespace, key, release_id, old_digest, new_digest, operator, status_code, details)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.nowText(), operation, namespace, key, releaseID, oldDigest, newDigest, operator, status, details,
	)
	if err != nil {
		log.Printf("record failure audit: %v", err)
	}
}

func (s *Store) ListAudit(ctx context.Context, namespace string, limit int) ([]model.AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `SELECT id, created_at, operation, namespace, key, release_id,
		old_digest, new_digest, operator, status_code, details
		FROM audit_log`
	args := make([]any, 0, 2)
	if namespace != "" {
		query += ` WHERE namespace = ?`
		args = append(args, namespace)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []model.AuditEntry
	for rows.Next() {
		var entry model.AuditEntry
		if err := rows.Scan(
			&entry.ID, &entry.CreatedAt, &entry.Operation, &entry.Namespace, &entry.Key,
			&entry.ReleaseID, &entry.OldDigest, &entry.NewDigest, &entry.Operator,
			&entry.StatusCode, &entry.Details,
		); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
