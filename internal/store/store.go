package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"meshdoctor/internal/errs"
)

var ErrInvalidSnapshot = errors.New("invalid checkpoint snapshot")

type Checkpoint struct {
	SampleID     string
	Stage        string
	StageIndex   int
	State        string
	ParamVersion string
	AlgoVersion  string
	ParamsJSON   string
	MeshBlob     []byte
	MeshHash     string
	SnapshotJSON []byte
	ErrorCode    string
	ErrorMessage string
	UpdatedAt    string
}

type StateRecord struct {
	SampleID     string
	Stage        string
	ParamVersion string
	AlgoVersion  string
	State        string
	ErrorCode    string
	ErrorMessage string
	UpdatedAt    string
}

type Store struct {
	db *sql.DB
	mu sync.Mutex

	Commits       atomic.Int64
	CacheHits     atomic.Int64
	CacheMisses   atomic.Int64
	Invalidations atomic.Int64
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS checkpoints (
  sample_id TEXT NOT NULL,
  stage TEXT NOT NULL,
  stage_index INTEGER NOT NULL,
  state TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  params_json TEXT NOT NULL,
  mesh_blob BLOB NOT NULL,
  mesh_hash TEXT NOT NULL,
  snapshot_json BLOB NOT NULL,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(sample_id, stage, param_version, algo_version)
) STRICT;
CREATE TABLE IF NOT EXISTS stage_states (
  sample_id TEXT NOT NULL,
  stage TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  state TEXT NOT NULL,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(sample_id, stage, param_version, algo_version)
) STRICT;
CREATE TABLE IF NOT EXISTS cache_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sample_id TEXT NOT NULL,
  event TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  at TEXT NOT NULL
);
`)
	return err
}

func (s *Store) LogEvent(ctx context.Context, sampleID, event, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO cache_events(sample_id,event,detail,at) VALUES(?,?,?,?)`,
		sampleID, event, detail, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) Events(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sample_id,event,detail,at FROM cache_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var sampleID, event, detail, at string
		if err := rows.Scan(&sampleID, &event, &detail, &at); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"sample_id": sampleID, "event": event, "detail": detail, "at": at})
	}
	return out, rows.Err()
}

// SaveState serializes a state-machine transition. It never rewrites a
// committed row through this method; callers must check Transition first.
func (s *Store) SaveState(ctx context.Context, rec StateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.UpdatedAt == "" {
		rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO stage_states(sample_id,stage,param_version,algo_version,state,error_code,error_message,updated_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(sample_id,stage,param_version,algo_version)
DO UPDATE SET state=excluded.state,error_code=excluded.error_code,error_message=excluded.error_message,updated_at=excluded.updated_at`,
		rec.SampleID, rec.Stage, rec.ParamVersion, rec.AlgoVersion, rec.State, rec.ErrorCode, rec.ErrorMessage, rec.UpdatedAt)
	return err
}

func (s *Store) GetState(ctx context.Context, sampleID, stage, pv, av string) (StateRecord, error) {
	var rec StateRecord
	err := s.db.QueryRowContext(ctx, `
SELECT sample_id,stage,param_version,algo_version,state,error_code,error_message,updated_at
FROM stage_states WHERE sample_id=? AND stage=? AND param_version=? AND algo_version=?`,
		sampleID, stage, pv, av).Scan(&rec.SampleID, &rec.Stage, &rec.ParamVersion, &rec.AlgoVersion,
		&rec.State, &rec.ErrorCode, &rec.ErrorMessage, &rec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return StateRecord{SampleID: sampleID, Stage: stage, ParamVersion: pv, AlgoVersion: av, State: "pending"}, nil
	}
	return rec, err
}

// CommitCheckpoint writes state and mesh blob in one SQLite transaction. The
// store mutex and MaxOpenConns(1) make commits serial and non-interleaving.
// An existing checkpoint with a different hash is frozen and rejected.
func (s *Store) CommitCheckpoint(ctx context.Context, cp Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cp.UpdatedAt == "" {
		cp.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var oldHash string
	err = tx.QueryRowContext(ctx, `SELECT mesh_hash FROM checkpoints WHERE sample_id=? AND stage=? AND param_version=? AND algo_version=?`,
		cp.SampleID, cp.Stage, cp.ParamVersion, cp.AlgoVersion).Scan(&oldHash)
	switch {
	case err == nil:
		if oldHash != cp.MeshHash {
			return errs.New(errs.ESTATE, "committed stage %s/%s is frozen; refusing silent rewrite", cp.SampleID, cp.Stage)
		}
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `
INSERT INTO checkpoints(sample_id,stage,stage_index,state,param_version,algo_version,params_json,mesh_blob,mesh_hash,snapshot_json,error_code,error_message,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			cp.SampleID, cp.Stage, cp.StageIndex, "committed", cp.ParamVersion, cp.AlgoVersion, cp.ParamsJSON,
			cp.MeshBlob, cp.MeshHash, cp.SnapshotJSON, cp.ErrorCode, cp.ErrorMessage, cp.UpdatedAt)
		if err != nil {
			return err
		}
	default:
		return err
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO stage_states(sample_id,stage,param_version,algo_version,state,error_code,error_message,updated_at)
VALUES(?,?,?,?, 'committed','','',?)
ON CONFLICT(sample_id,stage,param_version,algo_version)
DO UPDATE SET state='committed',error_code='',error_message='',updated_at=excluded.updated_at`,
		cp.SampleID, cp.Stage, cp.ParamVersion, cp.AlgoVersion, cp.UpdatedAt)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.Commits.Add(1)
	return nil
}

// LoadRecoveryChain validates every stored row for the sample. Any content
// hash or internal version corruption invalidates the whole sample snapshot.
// It then returns the contiguous committed prefix for the expected version.
func (s *Store) LoadRecoveryChain(ctx context.Context, sampleID, pv, av string) ([][]byte, []StateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx, `
SELECT stage,param_version,algo_version,mesh_blob,mesh_hash,snapshot_json
FROM checkpoints WHERE sample_id=? ORDER BY stage_index`, sampleID)
	if err != nil {
		return nil, nil, err
	}
	type raw struct {
		stage, pv, av, hash string
		blob, snapshot      []byte
	}
	var all []raw
	invalid := false
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.stage, &r.pv, &r.av, &r.blob, &r.hash, &r.snapshot); err != nil {
			rows.Close()
			return nil, nil, err
		}
		sum := sha256.Sum256(r.blob)
		actual := hex.EncodeToString(sum[:])
		if actual != r.hash || r.pv == "" || r.av == "" {
			invalid = true
		}
		var meta struct {
			ParamVersion string `json:"param_version"`
			AlgoVersion  string `json:"algo_version"`
		}
		if err := json.Unmarshal(r.snapshot, &meta); err != nil ||
			meta.ParamVersion != r.pv || meta.AlgoVersion != r.av ||
			r.pv != pv || r.av != av ||
			meta.ParamVersion != pv || meta.AlgoVersion != av {
			invalid = true
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if invalid {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE sample_id=?`, sampleID); err != nil {
			return nil, nil, err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM stage_states WHERE sample_id=?`, sampleID); err != nil {
			return nil, nil, err
		}
		s.Invalidations.Add(1)
		if err := s.insertEventLocked(ctx, sampleID, "invalidate", "hash/version mismatch; full recompute"); err != nil {
			return nil, nil, err
		}
		return nil, nil, ErrInvalidSnapshot
	}

	rows, err = s.db.QueryContext(ctx, `
SELECT stage,state,error_code,error_message,updated_at FROM stage_states
WHERE sample_id=? AND param_version=? AND algo_version=? ORDER BY stage`, sampleID, pv, av)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var states []StateRecord
	for rows.Next() {
		r := StateRecord{SampleID: sampleID, ParamVersion: pv, AlgoVersion: av}
		if err := rows.Scan(&r.Stage, &r.State, &r.ErrorCode, &r.ErrorMessage, &r.UpdatedAt); err != nil {
			return nil, nil, err
		}
		states = append(states, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var snapshots [][]byte
	expectedStage := 0
	for _, r := range all {
		if r.pv != pv || r.av != av {
			continue
		}
		stageIndex := stageOrder(r.stage)
		if stageIndex != expectedStage {
			break
		}
		snapshots = append(snapshots, append([]byte(nil), r.snapshot...))
		expectedStage++
		s.CacheHits.Add(1)
	}
	return snapshots, states, nil
}

func (s *Store) InvalidateSample(ctx context.Context, sampleID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE sample_id=?`, sampleID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM stage_states WHERE sample_id=?`, sampleID); err != nil {
		return err
	}
	s.Invalidations.Add(1)
	return s.insertEventLocked(ctx, sampleID, "invalidate", reason)
}

func (s *Store) insertEventLocked(ctx context.Context, sampleID, event, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cache_events(sample_id,event,detail,at) VALUES(?,?,?,?)`,
		sampleID, event, detail, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) CacheStats() map[string]any {
	return map[string]any{
		"commits":       s.Commits.Load(),
		"cache_hits":    s.CacheHits.Load(),
		"cache_misses":  s.CacheMisses.Load(),
		"invalidations": s.Invalidations.Load(),
	}
}

func stageOrder(stage string) int {
	switch stage {
	case "diagnostic":
		return 0
	case "weld":
		return 1
	case "fill":
		return 2
	case "orient":
		return 3
	case "smooth":
		return 4
	case "stats":
		return 5
	default:
		return -1
	}
}

func HashBlob(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}
