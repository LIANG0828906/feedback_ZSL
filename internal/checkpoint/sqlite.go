package checkpoint

import (
	"context"
	"database/sql"

	"msworkbench/internal/domain"
	"msworkbench/internal/e"
)

// SQLiteSchema is the integration DDL for a real SQLite database. Open the
// driver selected by your integration and pass *sql.DB to NewSQLiteStore.
const SQLiteSchema = `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS block_stage (
  run_id           TEXT NOT NULL,
  seq              INTEGER NOT NULL,
  block_coord      TEXT NOT NULL,
  stage            TEXT NOT NULL CHECK (stage IN ('quant','recon')),
  output_digest    TEXT NOT NULL,
  selected_method  TEXT NOT NULL,
  error_bound_bp   INTEGER NOT NULL,
  cache_key        TEXT NOT NULL,
  content_hash     TEXT NOT NULL,
  param_version    TEXT NOT NULL,
  algo_version     TEXT NOT NULL,
  payload_json     BLOB,
  committed_at     TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (run_id, block_coord, stage),
  UNIQUE (run_id, seq)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_block_stage_recover
  ON block_stage(run_id, seq);
`

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) *SQLiteStore { return &SQLiteStore{db: db} }

func (s *SQLiteStore) Init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, SQLiteSchema)
	return err
}

// CommitStage writes one stage in one transaction. The primary key and unique
// sequence prevent interleaved or duplicate stage commits.
func (s *SQLiteStore) CommitStage(ctx context.Context, r StageRecord, payload []byte) (StageRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StageRecord{}, err
	}
	defer tx.Rollback()
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM block_stage WHERE run_id=?`, r.RunID).Scan(&next); err != nil {
		return StageRecord{}, err
	}
	r.Seq = next
	r.Committed = true
	if r.ParamVersion == "" {
		r.ParamVersion = domain.ParamVersion
	}
	if r.AlgoVersion == "" {
		r.AlgoVersion = domain.AlgorithmVersion
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO block_stage
(run_id,seq,block_coord,stage,output_digest,selected_method,error_bound_bp,cache_key,content_hash,param_version,algo_version,payload_json)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.RunID, r.Seq, r.Block, r.Stage, r.OutputDigest, r.SelectedMethod, r.ErrorBoundBP,
		r.CacheKey, r.ContentHash, r.ParamVersion, r.AlgoVersion, payload)
	if err != nil {
		return StageRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return StageRecord{}, err
	}
	return r, nil
}

func (s *SQLiteStore) RecoverRun(ctx context.Context, runID string, expected map[string]string) ([]StageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id,seq,block_coord,stage,output_digest,selected_method,error_bound_bp,cache_key,content_hash,param_version,algo_version
FROM block_stage WHERE run_id=? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StageRecord
	for rows.Next() {
		var r StageRecord
		if err := rows.Scan(&r.RunID, &r.Seq, &r.Block, &r.Stage, &r.OutputDigest, &r.SelectedMethod, &r.ErrorBoundBP, &r.CacheKey, &r.ContentHash, &r.ParamVersion, &r.AlgoVersion); err != nil {
			return nil, err
		}
		if r.ParamVersion != domain.ParamVersion || r.AlgoVersion != domain.AlgorithmVersion {
			return nil, &e.Error{Code: e.Ver, Block: r.Block, Field: "version", Expected: domain.ParamVersion + "/" + domain.AlgorithmVersion, Actual: r.ParamVersion + "/" + r.AlgoVersion}
		}
		if want := expected[r.Block]; want != "" && r.ContentHash != want {
			return nil, &e.Error{Code: e.Ver, Block: r.Block, Field: "content_hash", Expected: want, Actual: r.ContentHash}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
