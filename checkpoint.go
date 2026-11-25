package audiofp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const CheckpointSchemaVersion = 1

type CheckpointIdentity struct {
	MaterialHash     string `json:"material_hash"`
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
}

type CheckpointRecord struct {
	WindowIndex       int         `json:"window_index"`
	StartSample       int         `json:"start_sample"`
	FingerprintDigest string      `json:"fingerprint_digest"`
	Fingerprint       Fingerprint `json:"fingerprint"`
	ParamVersion      string      `json:"param_version"`
	MaterialHash      string      `json:"material_hash"`
	AlgorithmVersion  string      `json:"algorithm_version"`
	CreatedAt         time.Time   `json:"created_at"`
}

type Snapshot struct {
	Identity    CheckpointIdentity `json:"identity"`
	Records     []CheckpointRecord `json:"records"`
	Reset       bool               `json:"reset"`
	ResetReason string             `json:"reset_reason,omitempty"`
}

type CheckpointStore struct {
	db       *sql.DB
	mu       sync.Mutex
	path     string
	identity CheckpointIdentity
}

// OpenAndResumeCheckpoint opens SQLite, applies versioned schema migrations and
// loads a compatible prefix. Any material/parameter/algorithm identity
// mismatch or corrupt fingerprint causes the whole snapshot to be deleted.
func OpenAndResumeCheckpoint(ctx context.Context, path string, identity CheckpointIdentity) (*CheckpointStore, *Snapshot, error) {
	if identity.AlgorithmVersion == "" {
		identity.AlgorithmVersion = AlgorithmVersion
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		_ = db.Close()
		return nil, nil, err
	}

	store := &CheckpointStore{db: db, path: path, identity: identity}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}

	snapshot, err := store.loadOrReset(ctx, identity)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return store, snapshot, nil
}

func (s *CheckpointStore) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS frame_windows (
			window_index INTEGER PRIMARY KEY,
			start_sample INTEGER NOT NULL,
			fingerprint_digest TEXT NOT NULL,
			fingerprint_json TEXT NOT NULL,
			param_version TEXT NOT NULL,
			material_hash TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	var applied int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version=?`, CheckpointSchemaVersion).Scan(&applied)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`,
			CheckpointSchemaVersion, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, CheckpointSchemaVersion)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return nil
}

func (s *CheckpointStore) loadOrReset(ctx context.Context, identity CheckpointIdentity) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, err := readIdentity(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		if err := resetAll(ctx, s.db, identity); err != nil {
			return nil, err
		}
		return &Snapshot{Identity: identity, Records: []CheckpointRecord{}}, nil
	}

	if *stored != identity {
		reasons := make([]string, 0, 3)
		if stored.MaterialHash != identity.MaterialHash {
			reasons = append(reasons, "material_hash")
		}
		if stored.ParamVersion != identity.ParamVersion {
			reasons = append(reasons, "param_version")
		}
		if stored.AlgorithmVersion != identity.AlgorithmVersion {
			reasons = append(reasons, "algorithm_version")
		}
		if err := resetAll(ctx, s.db, identity); err != nil {
			return nil, err
		}
		return &Snapshot{
			Identity: identity, Records: []CheckpointRecord{}, Reset: true,
			ResetReason: "identity mismatch: " + joinComma(reasons),
		}, nil
	}

	rows, err := s.db.QueryContext(ctx, `SELECT window_index,start_sample,fingerprint_digest,fingerprint_json,
		param_version,material_hash,algorithm_version,created_at FROM frame_windows ORDER BY window_index ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []CheckpointRecord{}
	for rows.Next() {
		var rec CheckpointRecord
		var fingerprintJSON, createdAt string
		if err := rows.Scan(&rec.WindowIndex, &rec.StartSample, &rec.FingerprintDigest, &fingerprintJSON,
			&rec.ParamVersion, &rec.MaterialHash, &rec.AlgorithmVersion, &createdAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(fingerprintJSON), &rec.Fingerprint); err != nil {
			return s.resetForCorrupt(ctx, identity, "invalid fingerprint_json at window "+fmt.Sprint(rec.WindowIndex))
		}
		if rec.Fingerprint.Digest() != rec.FingerprintDigest {
			return s.resetForCorrupt(ctx, identity, "digest mismatch at window "+fmt.Sprint(rec.WindowIndex))
		}
		rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, rec := range records {
		if rec.WindowIndex != i {
			return s.resetForCorrupt(ctx, identity, "non-contiguous window sequence")
		}
	}
	var next int
	if err := s.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM checkpoint_meta WHERE key='next_window_index'`).Scan(&next); err != nil {
		return s.resetForCorrupt(ctx, identity, "missing next_window_index")
	}
	if next != len(records) {
		return s.resetForCorrupt(ctx, identity, "next_window_index does not match record count")
	}
	return &Snapshot{Identity: identity, Records: records}, nil
}

func (s *CheckpointStore) resetForCorrupt(ctx context.Context, identity CheckpointIdentity, reason string) (*Snapshot, error) {
	if err := resetAll(ctx, s.db, identity); err != nil {
		return nil, err
	}
	return &Snapshot{Identity: identity, Records: []CheckpointRecord{}, Reset: true, ResetReason: reason}, nil
}

func readIdentity(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (*CheckpointIdentity, error) {
	keys := []string{"material_hash", "param_version", "algorithm_version"}
	values := make(map[string]string, 3)
	for _, key := range keys {
		var value string
		err := q.QueryRowContext(ctx, `SELECT value FROM checkpoint_meta WHERE key=?`, key).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		values[key] = value
	}
	return &CheckpointIdentity{
		MaterialHash: values["material_hash"], ParamVersion: values["param_version"],
		AlgorithmVersion: values["algorithm_version"],
	}, nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func upsertMeta(ctx context.Context, q execer, key, value string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO checkpoint_meta(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func resetAll(ctx context.Context, db *sql.DB, identity CheckpointIdentity) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM frame_windows`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_meta`); err != nil {
		return err
	}
	if err := upsertMeta(ctx, tx, "material_hash", identity.MaterialHash); err != nil {
		return err
	}
	if err := upsertMeta(ctx, tx, "param_version", identity.ParamVersion); err != nil {
		return err
	}
	if err := upsertMeta(ctx, tx, "algorithm_version", identity.AlgorithmVersion); err != nil {
		return err
	}
	if err := upsertMeta(ctx, tx, "next_window_index", "0"); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveWindow atomically commits the next expected window. A process-wide mutex
// and MaxOpenConns(1) serialize SQLite transactions.
func (s *CheckpointStore) SaveWindow(ctx context.Context, rec CheckpointRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.MaterialHash == "" {
		rec.MaterialHash = s.identity.MaterialHash
	}
	if rec.ParamVersion == "" {
		rec.ParamVersion = s.identity.ParamVersion
	}
	if rec.AlgorithmVersion == "" {
		rec.AlgorithmVersion = s.identity.AlgorithmVersion
	}
	if rec.MaterialHash != s.identity.MaterialHash || rec.ParamVersion != s.identity.ParamVersion ||
		rec.AlgorithmVersion != s.identity.AlgorithmVersion {
		return newError(CodeDiff, "checkpoint_identity", "refusing to write window with mismatched identity")
	}
	fingerprintJSON, err := json.Marshal(rec.Fingerprint)
	if err != nil {
		return err
	}
	digest := rec.Fingerprint.Digest()
	if rec.FingerprintDigest == "" {
		rec.FingerprintDigest = digest
	} else if rec.FingerprintDigest != digest {
		return newError(CodeDiff, "fingerprint_digest", "recorded digest does not match fingerprint")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var next int
	if err := tx.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM checkpoint_meta WHERE key='next_window_index'`).Scan(&next); err != nil {
		return err
	}
	if rec.WindowIndex != next {
		return newError(CodeState, "window_index", "checkpoint commits must be ordered: expected %d got %d", next, rec.WindowIndex)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO frame_windows
		(window_index,start_sample,fingerprint_digest,fingerprint_json,param_version,material_hash,algorithm_version,created_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		rec.WindowIndex, rec.StartSample, rec.FingerprintDigest, string(fingerprintJSON),
		rec.ParamVersion, rec.MaterialHash, rec.AlgorithmVersion, now)
	if err != nil {
		return err
	}
	if err := upsertMeta(ctx, tx, "next_window_index", fmt.Sprint(next+1)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CheckpointStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func joinComma(values []string) string {
	out := ""
	for i, v := range values {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
