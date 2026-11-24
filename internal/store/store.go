package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"audiocheck/internal/model"

	_ "modernc.org/sqlite"
)

type Store struct {
	db        *sql.DB
	path      string
	mu        sync.Mutex
	identity  model.Identity
	commitSeq int
}

type OpenResult struct {
	Store          *Store
	Reset          bool
	ResetReason    string
	ExistingBlocks int
}

func Open(path string, identity model.Identity) (*OpenResult, error) {
	if path == "" {
		return nil, model.NewError(model.CodeSchema, "db_path", "empty SQLite path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
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
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path, identity: identity}
	if err := s.createSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}

	res := &OpenResult{Store: s}
	stored, err := s.loadIdentity(context.Background())
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		if err := s.writeIdentity(context.Background(), identity); err != nil {
			return nil, err
		}
	} else {
		reason := mismatchReason(stored, identity)
		if reason != "" {
			var count int
			_ = s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM blocks`).Scan(&count)
			res.ExistingBlocks = count
			res.Reset = true
			res.ResetReason = reason
			if err := s.resetLockedSchema(context.Background(), identity); err != nil {
				return nil, err
			}
		} else if stored["param_hash"] != identity.ParamHash {
			if err := s.upsertMeta(context.Background(), "param_hash", identity.ParamHash); err != nil {
				return nil, err
			}
		}
	}
	seq, err := s.loadMaxCommitSeq(context.Background())
	if err != nil {
		return nil, err
	}
	s.commitSeq = seq
	return res, nil
}

func mismatchReason(stored map[string]string, want model.Identity) string {
	checks := []struct {
		key  string
		want string
	}{
		{"content_hash", want.ContentHash},
		{"param_version", want.ParamVersion},
		{"algorithm_version", want.AlgorithmVersion},
		{"schema_version", want.SchemaVersion},
		{"truth_fingerprint", want.TruthFingerprint},
	}
	for _, c := range checks {
		got, ok := stored[c.key]
		if !ok || got == "" {
			return fmt.Sprintf("missing identity %s", c.key)
		}
		if got != c.want {
			return fmt.Sprintf("%s mismatch: snapshot=%s current=%s", c.key, got, c.want)
		}
	}
	return ""
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS snapshot_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS blocks (
  block_index INTEGER PRIMARY KEY,
  first_frame INTEGER NOT NULL,
  last_frame INTEGER NOT NULL,
  coord_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  block_hash TEXT NOT NULL,
  recon_digest TEXT NOT NULL DEFAULT '',
  param_version TEXT NOT NULL,
  param_hash TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  status TEXT NOT NULL,
  commit_seq INTEGER,
  payload_json TEXT NOT NULL,
  committed_at_ns INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_blocks_commit_seq ON blocks(commit_seq);
CREATE TABLE IF NOT EXISTS frame_metrics (
  block_index INTEGER NOT NULL,
  frame_index INTEGER NOT NULL,
  psnr_db_fixed INTEGER NOT NULL,
  snr_db_fixed INTEGER NOT NULL,
  ssim_fixed INTEGER NOT NULL,
  noise_std_fixed INTEGER NOT NULL,
  bound_rms_upper_fixed INTEGER NOT NULL,
  snr_upper_fixed INTEGER NOT NULL,
  PRIMARY KEY(block_index, frame_index)
);
CREATE TABLE IF NOT EXISTS block_states (
  block_index INTEGER NOT NULL,
  stage TEXT NOT NULL,
  state TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  updated_ns INTEGER NOT NULL,
  PRIMARY KEY(block_index, stage)
);
CREATE TABLE IF NOT EXISTS state_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  block_index INTEGER NOT NULL,
  stage TEXT NOT NULL,
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  at_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS queue_records (
  block_index INTEGER PRIMARY KEY,
  reason TEXT NOT NULL DEFAULT '',
  enqueue_wait_ns INTEGER NOT NULL,
  schedule_wait_ns INTEGER NOT NULL,
  total_wait_ns INTEGER NOT NULL,
  worker_id INTEGER NOT NULL,
  queue_depth INTEGER NOT NULL,
  buffer_samples INTEGER NOT NULL,
  max_buffer_samples INTEGER NOT NULL
);
`

func (s *Store) createSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schemaSQL)
	return err
}

func (s *Store) resetLockedSchema(ctx context.Context, id model.Identity) error {
	_, err := s.db.ExecContext(ctx, `
DROP TABLE IF EXISTS frame_metrics;
DROP TABLE IF EXISTS queue_records;
DROP TABLE IF EXISTS state_events;
DROP TABLE IF EXISTS block_states;
DROP TABLE IF EXISTS blocks;
DROP TABLE IF EXISTS snapshot_meta;
`)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	return s.writeIdentity(ctx, id)
}

func (s *Store) Reset(ctx context.Context, id model.Identity, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resetLockedSchema(ctx, id); err != nil {
		return err
	}
	s.identity = id
	s.commitSeq = 0
	return nil
}

func (s *Store) writeIdentity(ctx context.Context, id model.Identity) error {
	rows := [][2]string{
		{"content_hash", id.ContentHash},
		{"original_hash", id.OriginalHash},
		{"reconstruction_hash", id.ReconstructionHash},
		{"truth_fingerprint", id.TruthFingerprint},
		{"param_version", id.ParamVersion},
		{"param_hash", id.ParamHash},
		{"algorithm_version", id.AlgorithmVersion},
		{"schema_version", id.SchemaVersion},
	}
	for _, kv := range rows {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO snapshot_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) upsertMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO snapshot_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) loadIdentity(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM snapshot_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) Identity() model.Identity { return s.identity }

func (s *Store) loadMaxCommitSeq(ctx context.Context) (int, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(commit_seq) FROM blocks`).Scan(&seq); err != nil {
		return 0, err
	}
	return int(seq.Int64), nil
}

func (s *Store) SaveState(ctx context.Context, blockIndex int, st model.StageState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveStateLocked(ctx, blockIndex, st, "")
}

func (s *Store) saveStateLocked(ctx context.Context, blockIndex int, st model.StageState, from string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO block_states(block_index,stage,state,detail,updated_ns)
VALUES(?,?,?,?,?)
ON CONFLICT(block_index,stage) DO UPDATE SET state=excluded.state, detail=excluded.detail, updated_ns=excluded.updated_ns`,
		blockIndex, st.Stage, st.State, st.Detail, st.UpdatedNS)
	if err != nil {
		return err
	}
	if from != "" && from != st.State {
		_, err = s.db.ExecContext(ctx, `INSERT INTO state_events(block_index,stage,from_state,to_state,detail,at_ns) VALUES(?,?,?,?,?,?)`,
			blockIndex, st.Stage, from, st.State, st.Detail, st.UpdatedNS)
	}
	return err
}

func (s *Store) CommitBlock(ctx context.Context, block model.BlockResult) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	payload, err := json.Marshal(block)
	if err != nil {
		return 0, err
	}
	coordJSON, err := json.Marshal(block.Block)
	if err != nil {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var existingSeq sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT commit_seq FROM blocks WHERE block_index=?`, block.Block.Index).Scan(&existingSeq)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	seq := int(existingSeq.Int64)
	if !existingSeq.Valid {
		s.commitSeq++
		seq = s.commitSeq
	}
	now := time.Now().UnixNano()
	status := StateCommitted
	_, err = tx.ExecContext(ctx, `
INSERT INTO blocks(block_index,first_frame,last_frame,coord_json,content_hash,block_hash,recon_digest,param_version,param_hash,algorithm_version,status,commit_seq,payload_json,committed_at_ns)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(block_index) DO UPDATE SET
 first_frame=excluded.first_frame,last_frame=excluded.last_frame,coord_json=excluded.coord_json,
 content_hash=excluded.content_hash,block_hash=excluded.block_hash,param_version=excluded.param_version,
 recon_digest=excluded.recon_digest,param_version=excluded.param_version,
 param_hash=excluded.param_hash,algorithm_version=excluded.algorithm_version,status=excluded.status,
 commit_seq=excluded.commit_seq,payload_json=excluded.payload_json,committed_at_ns=excluded.committed_at_ns`,
		block.Block.Index, block.Block.FirstFrame, block.Block.LastFrame, string(coordJSON),
		block.ContentHash, block.BlockHash, block.ReconSummary.SHA256, block.ParamVersion, block.ParamHash, block.AlgorithmVersion,
		status, seq, string(payload), now)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM frame_metrics WHERE block_index=?`, block.Block.Index); err != nil {
		return 0, err
	}
	for _, fr := range block.Frames {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO frame_metrics(block_index,frame_index,psnr_db_fixed,snr_db_fixed,ssim_fixed,noise_std_fixed,bound_rms_upper_fixed,snr_upper_fixed)
VALUES(?,?,?,?,?,?,?,?)`, block.Block.Index, fr.Frame, fr.PSNRDBFixed, fr.SNRDBFixed, fr.SSIMFixed, fr.NoiseFloorStdFixed, fr.Bound.RMSErrorUpperFixed, fr.Bound.SNRUpperDBFixed); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM queue_records WHERE block_index=?`, block.Block.Index); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO queue_records(block_index,reason,enqueue_wait_ns,schedule_wait_ns,total_wait_ns,worker_id,queue_depth,buffer_samples,max_buffer_samples)
VALUES(?,?,?,?,?,?,?,?,?)`, block.Block.Index, block.Queue.Reason, block.Queue.EnqueueWaitNS, block.Queue.ScheduleWaitNS,
		block.Queue.TotalWaitNS, block.Queue.WorkerID, block.Queue.QueueDepthAtSubmit, block.Queue.BufferSamples, block.Queue.MaxBufferSamples); err != nil {
		return 0, err
	}
	for _, st := range block.States {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO block_states(block_index,stage,state,detail,updated_ns)
VALUES(?,?,?,?,?)
ON CONFLICT(block_index,stage) DO UPDATE SET state=excluded.state,detail=excluded.detail,updated_ns=excluded.updated_ns`,
			block.Block.Index, st.Stage, st.State, st.Detail, st.UpdatedNS); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	block.CommitSeq = seq
	return seq, nil
}

const StateCommitted = "committed"

func (s *Store) ListBlocks(ctx context.Context) ([]model.BlockResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload_json FROM blocks ORDER BY block_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.BlockResult
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var b model.BlockResult
		if err := json.Unmarshal([]byte(payload), &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) CommitOrder(ctx context.Context) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT block_index FROM blocks ORDER BY commit_seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, err
		}
		out = append(out, idx)
	}
	return out, rows.Err()
}

func (s *Store) PruneBlocks(ctx context.Context, keepCount int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"frame_metrics", "queue_records", "block_states", "state_events", "blocks"} {
		column := "block_index"
		if table == "blocks" {
			column = "block_index"
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+column+" >= ?", keepCount); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) StateMatrix(ctx context.Context) (map[int][]model.StageState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT block_index,stage,state,detail,updated_ns FROM block_states ORDER BY block_index, stage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int][]model.StageState{}
	for rows.Next() {
		var idx int
		var st model.StageState
		if err := rows.Scan(&idx, &st.Stage, &st.State, &st.Detail, &st.UpdatedNS); err != nil {
			return nil, err
		}
		out[idx] = append(out[idx], st)
	}
	return out, rows.Err()
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}
