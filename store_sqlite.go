package maskqa

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Checkpoint struct {
	Seq          int64     `json:"seq"`
	SampleID     string    `json:"sample_id"`
	BlockX       int       `json:"block_x"`
	BlockY       int       `json:"block_y"`
	Stage        Stage     `json:"stage"`
	Iteration    int       `json:"iteration"`
	SummaryJSON  string    `json:"summary_json"`
	RadiusJSON   string    `json:"radius_json"`
	CacheKey     string    `json:"cache_key"`
	ContentHash  string    `json:"content_hash"`
	ParamVersion string    `json:"param_version"`
	AlgoVersion  string    `json:"algo_version"`
	OutputJSON   string    `json:"output_json,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type StateRecord struct {
	SampleID     string    `json:"sample_id"`
	BlockX       int       `json:"block_x"`
	BlockY       int       `json:"block_y"`
	Stage        Stage     `json:"stage"`
	State        State     `json:"state"`
	Attempts     int       `json:"attempts"`
	LastError    string    `json:"last_error"`
	CacheKey     string    `json:"cache_key"`
	ContentHash  string    `json:"content_hash"`
	ParamVersion string    `json:"param_version"`
	AlgoVersion  string    `json:"algo_version"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type RecoveryReport struct {
	FirstBadSeq     int64 `json:"first_bad_seq"`
	DeletedCommits  int64 `json:"deleted_commits"`
	ResetProcessing int64 `json:"reset_processing"`
	LastCompleteSeq int64 `json:"last_complete_seq"`
}

type Store struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

func OpenStore(path string) (*Store, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS block_state (
  sample_id TEXT NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  stage TEXT NOT NULL,
  state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  cache_key TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  param_version TEXT NOT NULL DEFAULT '',
  algo_version TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(sample_id, block_x, block_y, stage)
);
CREATE TABLE IF NOT EXISTS commits (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  sample_id TEXT NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  stage TEXT NOT NULL,
  iteration INTEGER NOT NULL DEFAULT 1,
  output_summary TEXT NOT NULL,
  radius_table TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  output_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_commits_sample_seq ON commits(sample_id, seq);
CREATE TABLE IF NOT EXISTS invalidations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sample_id TEXT NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  stage TEXT NOT NULL,
  reason TEXT NOT NULL,
  param_field TEXT NOT NULL DEFAULT '',
  upstream_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL
);`)
	return err
}

func blockCoords(b BlockCoord) (int, int) { return b.IndexX, b.IndexY }

func (s *Store) GetState(ctx context.Context, sampleID string, b BlockCoord, stage Stage) (*StateRecord, error) {
	bx, by := blockCoords(b)
	row := s.db.QueryRowContext(ctx, `SELECT sample_id,block_x,block_y,stage,state,attempts,last_error,cache_key,content_hash,param_version,algo_version,updated_at FROM block_state WHERE sample_id=? AND block_x=? AND block_y=? AND stage=?`, sampleID, bx, by, stage)
	var rec StateRecord
	var updated string
	err := row.Scan(&rec.SampleID, &rec.BlockX, &rec.BlockY, &rec.Stage, &rec.State, &rec.Attempts, &rec.LastError, &rec.CacheKey, &rec.ContentHash, &rec.ParamVersion, &rec.AlgoVersion, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &rec, nil
}

func (s *Store) Transition(ctx context.Context, sampleID string, b BlockCoord, stage Stage, next State, lastErr string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	bx, by := blockCoords(b)
	rec, err := s.GetState(ctx, sampleID, b, stage)
	if err != nil {
		return err
	}
	current := StatePending
	attempts := 0
	cacheKey, hash, pv, av := "", "", "", AlgoVersion
	if rec != nil {
		current = rec.State
		attempts = rec.Attempts
		cacheKey, hash, pv, av = rec.CacheKey, rec.ContentHash, rec.ParamVersion, rec.AlgoVersion
	}
	state, terr := TransitionState(current, next)
	if terr != nil {
		if qe, ok := terr.(*QAError); ok {
			qe.ObjectID = NodeRef{SampleID: sampleID, Block: b, Stage: stage}.Key()
			return qe
		}
		return terr
	}
	if next == StateProcessing {
		attempts++
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO block_state(sample_id,block_x,block_y,stage,state,attempts,last_error,cache_key,content_hash,param_version,algo_version,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(sample_id,block_x,block_y,stage) DO UPDATE SET state=excluded.state,attempts=excluded.attempts,last_error=excluded.last_error,updated_at=excluded.updated_at`,
		sampleID, bx, by, stage, state, attempts, lastErr, cacheKey, hash, pv, av, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) CommitCheckpoint(ctx context.Context, cp Checkpoint) (*Checkpoint, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if cp.AlgoVersion == "" {
		cp.AlgoVersion = AlgoVersion
	}
	if cp.Iteration == 0 {
		cp.Iteration = 1
	}
	now := time.Now().UTC()
	cp.CreatedAt = now
	bx, by := cp.BlockX, cp.BlockY
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	insertRes, err := tx.ExecContext(ctx, `INSERT INTO commits(sample_id,block_x,block_y,stage,iteration,output_summary,radius_table,cache_key,content_hash,param_version,algo_version,output_json,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, cp.SampleID, bx, by, cp.Stage, cp.Iteration, cp.SummaryJSON, cp.RadiusJSON, cp.CacheKey, cp.ContentHash, cp.ParamVersion, cp.AlgoVersion, cp.OutputJSON, now.Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE block_state SET state=?,last_error='',cache_key=?,content_hash=?,param_version=?,algo_version=?,updated_at=? WHERE sample_id=? AND block_x=? AND block_y=? AND stage=?`, StateCommitted, cp.CacheKey, cp.ContentHash, cp.ParamVersion, cp.AlgoVersion, now.Format(time.RFC3339Nano), cp.SampleID, bx, by, cp.Stage)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO block_state(sample_id,block_x,block_y,stage,state,attempts,last_error,cache_key,content_hash,param_version,algo_version,updated_at) VALUES(?,?,?,?,?,1,'',?,?,?,?,?)`, cp.SampleID, bx, by, cp.Stage, StateCommitted, cp.CacheKey, cp.ContentHash, cp.ParamVersion, cp.AlgoVersion, now.Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	cp.Seq, _ = insertRes.LastInsertId()
	return &cp, nil
}

func (s *Store) LatestCheckpoint(ctx context.Context, sampleID string, b BlockCoord, stage Stage) (*Checkpoint, error) {
	bx, by := blockCoords(b)
	row := s.db.QueryRowContext(ctx, `SELECT seq,sample_id,block_x,block_y,stage,iteration,output_summary,radius_table,cache_key,content_hash,param_version,algo_version,output_json,created_at FROM commits WHERE sample_id=? AND block_x=? AND block_y=? AND stage=? ORDER BY seq DESC LIMIT 1`, sampleID, bx, by, stage)
	return scanCheckpoint(row)
}

type rowScanner interface{ Scan(...any) error }

func scanCheckpoint(row rowScanner) (*Checkpoint, error) {
	var cp Checkpoint
	var created string
	err := row.Scan(&cp.Seq, &cp.SampleID, &cp.BlockX, &cp.BlockY, &cp.Stage, &cp.Iteration, &cp.SummaryJSON, &cp.RadiusJSON, &cp.CacheKey, &cp.ContentHash, &cp.ParamVersion, &cp.AlgoVersion, &cp.OutputJSON, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cp.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return &cp, nil
}

func (s *Store) ListCheckpoints(ctx context.Context, sampleID string) ([]Checkpoint, error) {
	q := `SELECT seq,sample_id,block_x,block_y,stage,iteration,output_summary,radius_table,cache_key,content_hash,param_version,algo_version,output_json,created_at FROM commits`
	args := []any{}
	if sampleID != "" {
		q += ` WHERE sample_id=?`
		args = append(args, sampleID)
	}
	q += ` ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		cp, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *cp)
	}
	return out, rows.Err()
}

func (s *Store) ListStates(ctx context.Context, sampleID string) ([]StateRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sample_id,block_x,block_y,stage,state,attempts,last_error,cache_key,content_hash,param_version,algo_version,updated_at FROM block_state WHERE (?='' OR sample_id=?) ORDER BY sample_id,block_y,block_x,stage`, sampleID, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateRecord
	for rows.Next() {
		var rec StateRecord
		var updated string
		if err := rows.Scan(&rec.SampleID, &rec.BlockX, &rec.BlockY, &rec.Stage, &rec.State, &rec.Attempts, &rec.LastError, &rec.CacheKey, &rec.ContentHash, &rec.ParamVersion, &rec.AlgoVersion, &updated); err != nil {
			return nil, err
		}
		rec.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) AddInvalidations(ctx context.Context, items []InvalidationItem) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, it := range items {
		up, _ := json.Marshal(it.Upstream)
		bx, by := blockCoords(it.Block)
		if _, err := s.db.ExecContext(ctx, `INSERT INTO invalidations(sample_id,block_x,block_y,stage,reason,param_field,upstream_json,created_at) VALUES(?,?,?,?,?,?,?,?)`, it.SampleID, bx, by, it.Stage, it.Reason, it.ParamField, string(up), now); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE block_state SET state=?,updated_at=? WHERE sample_id=? AND block_x=? AND block_y=? AND stage=? AND state=?`, StatePending, now, it.SampleID, bx, by, it.Stage, StateCommitted); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListInvalidations(ctx context.Context, sampleID string) ([]InvalidationItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sample_id,block_x,block_y,stage,reason,param_field,upstream_json FROM invalidations WHERE (?='' OR sample_id=?) ORDER BY id ASC`, sampleID, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InvalidationItem
	for rows.Next() {
		var it InvalidationItem
		var upstream string
		var bx, by int
		if err := rows.Scan(&it.SampleID, &bx, &by, &it.Stage, &it.Reason, &it.ParamField, &upstream); err != nil {
			return nil, err
		}
		it.Block.IndexX = bx
		it.Block.IndexY = by
		_ = json.Unmarshal([]byte(upstream), &it.Upstream)
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) RecoverIntegrity(ctx context.Context, expected map[Stage]string) (*RecoveryReport, error) {
	report := &RecoveryReport{}
	commits, err := s.ListCheckpoints(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, cp := range commits {
		bad := HashBytes("maskqa-content-v1", []byte(cp.OutputJSON)) != cp.ContentHash || cp.AlgoVersion != AlgoVersion
		if !bad && expected != nil {
			if pv, ok := expected[cp.Stage]; ok && pv != cp.ParamVersion {
				bad = true
			}
		}
		if bad {
			report.FirstBadSeq = cp.Seq
			break
		}
		report.LastCompleteSeq = cp.Seq
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if report.FirstBadSeq > 0 {
		res, err := s.db.ExecContext(ctx, `DELETE FROM commits WHERE seq>=?`, report.FirstBadSeq)
		if err != nil {
			return nil, err
		}
		n, _ := res.RowsAffected()
		report.DeletedCommits = n
		if _, err := s.db.ExecContext(ctx, `UPDATE block_state SET state=? WHERE state=?`, StatePending, StateCommitted); err != nil {
			return nil, err
		}
		remaining, err := s.ListCheckpoints(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, cp := range remaining {
			if _, err := s.db.ExecContext(ctx, `UPDATE block_state SET state=?,cache_key=?,content_hash=?,param_version=?,algo_version=? WHERE sample_id=? AND block_x=? AND block_y=? AND stage=?`, StateCommitted, cp.CacheKey, cp.ContentHash, cp.ParamVersion, cp.AlgoVersion, cp.SampleID, cp.BlockX, cp.BlockY, cp.Stage); err != nil {
				return nil, err
			}
		}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE block_state SET state=? WHERE state=?`, StatePending, StateProcessing)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	report.ResetProcessing = n
	return report, nil
}
