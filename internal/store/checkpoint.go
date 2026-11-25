package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
	"edgerefine/internal/state"

	_ "modernc.org/sqlite"
)

type StageRecord struct {
	SampleID         string
	Tile             model.TileCoord
	Stage            state.Stage
	Status           state.Status
	Summary          string
	Convergence      any
	Output           any
	ContentHash      string
	ParamVersion     int
	IterBudget       int
	AlgorithmVersion string
	TruthFingerprint string
}

type StoredStage struct {
	StageRecord
	OutputJSON       []byte
	ConvergenceJSON  []byte
	SequencePosition int64
	UpdatedAt        string
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite pragmas: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS schema_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT OR IGNORE INTO schema_meta(key,value) VALUES('schema_version', ?);
CREATE TABLE IF NOT EXISTS stage_checkpoints (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  sample_id TEXT NOT NULL,
  tx INTEGER NOT NULL,
  ty INTEGER NOT NULL,
  stage TEXT NOT NULL,
  status TEXT NOT NULL,
  summary TEXT NOT NULL,
  convergence_json BLOB NOT NULL,
  output_json BLOB NOT NULL,
  content_hash TEXT NOT NULL,
  param_version INTEGER NOT NULL,
  iter_budget INTEGER NOT NULL,
  algorithm_version TEXT NOT NULL,
  truth_fingerprint TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(sample_id,tx,ty,stage)
);
CREATE INDEX IF NOT EXISTS idx_stage_sample ON stage_checkpoints(sample_id, tx, ty, seq);
`, fmt.Sprint(model.SchemaVersion))
	return err
}

func (s *Store) SetMeta(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO schema_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM schema_meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetTruthFingerprint(sampleID, fp string) error {
	return s.SetMeta("truth_fingerprint:"+sampleID, fp)
}

func (s *Store) CommitStage(rec StageRecord) error {
	outputJSON, err := json.Marshal(rec.Output)
	if err != nil {
		return err
	}
	convJSON, err := json.Marshal(rec.Convergence)
	if err != nil {
		return err
	}
	hash := model.HashBytes("stage-output-v1:", outputJSON)
	if rec.ContentHash != "" && rec.ContentHash != hash {
		return errs.New(errs.ECheckpt, "precomputed content hash does not match serialized output")
	}
	rec.ContentHash = hash
	now := time.Now().UTC().Format(time.RFC3339Nano)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
INSERT INTO stage_checkpoints
(sample_id,tx,ty,stage,status,summary,convergence_json,output_json,content_hash,param_version,iter_budget,algorithm_version,truth_fingerprint,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(sample_id,tx,ty,stage) DO UPDATE SET
 status=excluded.status, summary=excluded.summary,
 convergence_json=excluded.convergence_json, output_json=excluded.output_json,
 content_hash=excluded.content_hash, param_version=excluded.param_version,
 iter_budget=excluded.iter_budget, algorithm_version=excluded.algorithm_version,
 truth_fingerprint=excluded.truth_fingerprint, updated_at=excluded.updated_at
`, rec.SampleID, rec.Tile.TX, rec.Tile.TY, string(rec.Stage), string(rec.Status), rec.Summary, convJSON, outputJSON,
		rec.ContentHash, rec.ParamVersion, rec.IterBudget, rec.AlgorithmVersion, rec.TruthFingerprint, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) InvalidateSample(sampleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM stage_checkpoints WHERE sample_id=?`, sampleID)
	return err
}

func (s *Store) LoadStages(sampleID string) ([]StoredStage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
SELECT seq,tx,ty,stage,status,summary,convergence_json,output_json,content_hash,param_version,iter_budget,algorithm_version,truth_fingerprint,updated_at
FROM stage_checkpoints WHERE sample_id=? ORDER BY tx,ty,stage`, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredStage
	for rows.Next() {
		var r StoredStage
		var stage string
		var status string
		if err := rows.Scan(&r.SequencePosition, &r.Tile.TX, &r.Tile.TY, &stage, &status, &r.Summary,
			&r.ConvergenceJSON, &r.OutputJSON, &r.ContentHash, &r.ParamVersion, &r.IterBudget,
			&r.AlgorithmVersion, &r.TruthFingerprint, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Stage = state.Stage(stage)
		r.Status = state.Status(status)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CommitSequence(sampleID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT tx,ty,stage FROM stage_checkpoints WHERE sample_id=? AND status='committed' ORDER BY seq`, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seq := []string{}
	for rows.Next() {
		var tx, ty int
		var stage string
		if err := rows.Scan(&tx, &ty, &stage); err != nil {
			return nil, err
		}
		seq = append(seq, fmt.Sprintf("%d,%d:%s", tx, ty, stage))
	}
	return seq, rows.Err()
}

// LoadValidStages verifies every hash/version. Any failure invalidates the
// whole sample snapshot so callers perform a full recomputation.
func (s *Store) LoadValidStages(sampleID, truthFingerprint string) ([]StoredStage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
SELECT seq,tx,ty,stage,status,summary,convergence_json,output_json,content_hash,param_version,iter_budget,algorithm_version,truth_fingerprint,updated_at
FROM stage_checkpoints WHERE sample_id=? AND status='committed' ORDER BY tx,ty,stage`, sampleID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var stages []StoredStage
	for rows.Next() {
		var r StoredStage
		var stage, status string
		if err := rows.Scan(&r.SequencePosition, &r.Tile.TX, &r.Tile.TY, &stage, &status, &r.Summary,
			&r.ConvergenceJSON, &r.OutputJSON, &r.ContentHash, &r.ParamVersion, &r.IterBudget,
			&r.AlgorithmVersion, &r.TruthFingerprint, &r.UpdatedAt); err != nil {
			return nil, false, err
		}
		r.Stage = state.Stage(stage)
		r.Status = state.Status(status)
		stages = append(stages, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	for _, st := range stages {
		want := model.HashBytes("stage-output-v1:", st.OutputJSON)
		if want != st.ContentHash || st.AlgorithmVersion != model.AlgorithmVersion {
			_, _ = s.db.Exec(`DELETE FROM stage_checkpoints WHERE sample_id=?`, sampleID)
			return nil, false, nil
		}
		if truthFingerprint != "" && st.TruthFingerprint != truthFingerprint {
			_, _ = s.db.Exec(`DELETE FROM stage_checkpoints WHERE sample_id=?`, sampleID)
			return nil, false, nil
		}
	}
	var dbFP string
	_ = s.db.QueryRow(`SELECT value FROM schema_meta WHERE key=?`, "truth_fingerprint:"+sampleID).Scan(&dbFP)
	if truthFingerprint != "" && dbFP != truthFingerprint {
		_, _ = s.db.Exec(`DELETE FROM stage_checkpoints WHERE sample_id=?`, sampleID)
		return nil, false, nil
	}
	return stages, true, nil
}
