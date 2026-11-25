package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/core"
)

type SQLiteStore struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, fmt.Errorf("empty database path")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
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
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA synchronous=FULL`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &SQLiteStore{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) init() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS runs (
  run_id TEXT NOT NULL,
  image_id TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  chain_fingerprint TEXT NOT NULL,
  project_fingerprint TEXT NOT NULL,
  truth_fingerprint TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  channels INTEGER NOT NULL,
  block_size INTEGER NOT NULL,
  step_count INTEGER NOT NULL,
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(run_id, image_id)
);
CREATE TABLE IF NOT EXISTS block_steps (
  run_id TEXT NOT NULL,
  image_id TEXT NOT NULL,
  step INTEGER NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  block_w INTEGER NOT NULL,
  block_h INTEGER NOT NULL,
  state TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  output_hash TEXT NOT NULL,
  output_digest TEXT NOT NULL,
  chain_fingerprint TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  pixels BLOB NOT NULL,
  commit_seq INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(run_id, image_id, step, block_x, block_y)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_block_steps_commit_seq ON block_steps(commit_seq);
CREATE INDEX IF NOT EXISTS idx_block_steps_cache ON block_steps(cache_key, algorithm_version);
CREATE TABLE IF NOT EXISTS block_manifests (
  run_id TEXT NOT NULL,
  image_id TEXT NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  block_w INTEGER NOT NULL,
  block_h INTEGER NOT NULL,
  chain_fingerprint TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  steps_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  commit_seq INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(run_id,image_id,block_x,block_y)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_block_manifests_commit_seq ON block_manifests(commit_seq);
CREATE TABLE IF NOT EXISTS run_events (
  run_id TEXT NOT NULL,
  image_id TEXT NOT NULL,
  event TEXT NOT NULL,
  detail TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
`)
	return err
}

func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func (s *SQLiteStore) EnsureRun(ctx context.Context, meta checkpoint.RunMeta, reset bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if reset {
		if _, err := tx.ExecContext(ctx, `DELETE FROM block_steps WHERE run_id=? AND image_id=?`,
			meta.RunID, meta.ImageID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM block_manifests WHERE run_id=? AND image_id=?`,
			meta.RunID, meta.ImageID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE run_id=? AND image_id=?`,
			meta.RunID, meta.ImageID); err != nil {
			return err
		}
	}
	if meta.Status == "" {
		meta.Status = "running"
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO runs(run_id,image_id,algorithm_version,chain_fingerprint,project_fingerprint,
 truth_fingerprint,width,height,channels,block_size,step_count,status,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(run_id,image_id) DO UPDATE SET
 updated_at=excluded.updated_at,
 status=CASE WHEN runs.status='completed' THEN runs.status ELSE excluded.status END`,
		meta.RunID, meta.ImageID, meta.AlgorithmVersion, meta.ChainFingerprint,
		meta.ProjectFingerprint, meta.TruthFingerprint, meta.Width, meta.Height,
		meta.Channels, meta.BlockSize, meta.StepCount, meta.Status, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetRunMeta(ctx context.Context, runID, imageID string) (checkpoint.RunMeta, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRowContext(ctx, `
SELECT run_id,image_id,algorithm_version,chain_fingerprint,project_fingerprint,
 truth_fingerprint,width,height,channels,block_size,step_count,status
FROM runs WHERE run_id=? AND image_id=?`, runID, imageID)
	var m checkpoint.RunMeta
	if err := row.Scan(&m.RunID, &m.ImageID, &m.AlgorithmVersion, &m.ChainFingerprint,
		&m.ProjectFingerprint, &m.TruthFingerprint, &m.Width, &m.Height, &m.Channels,
		&m.BlockSize, &m.StepCount, &m.Status); err != nil {
		if err == sql.ErrNoRows {
			return m, false, nil
		}
		return m, false, err
	}
	return m, true, nil
}

func (s *SQLiteStore) SetRunStatus(ctx context.Context, runID, imageID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?, updated_at=? WHERE run_id=? AND image_id=?`,
		status, time.Now().UnixNano(), runID, imageID)
	return err
}

func (s *SQLiteStore) CommitArtifact(ctx context.Context, a checkpoint.Artifact) (checkpoint.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	compressed, err := compress(a.Pixels)
	if err != nil {
		return a, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM (
SELECT MAX(commit_seq) AS seq FROM block_steps
UNION ALL SELECT MAX(commit_seq) AS seq FROM block_manifests)`).Scan(&seq); err != nil {
		return a, err
	}
	a.CommitSeq = seq
	now := time.Now().UnixNano()
	_, err = tx.ExecContext(ctx, `
INSERT INTO block_steps(run_id,image_id,step,block_x,block_y,block_w,block_h,state,
 cache_key,input_hash,output_hash,output_digest,chain_fingerprint,algorithm_version,
 pixels,commit_seq,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(run_id,image_id,step,block_x,block_y) DO UPDATE SET
 block_w=excluded.block_w, block_h=excluded.block_h, state=excluded.state,
 cache_key=excluded.cache_key, input_hash=excluded.input_hash,
 output_hash=excluded.output_hash, output_digest=excluded.output_digest,
 chain_fingerprint=excluded.chain_fingerprint, algorithm_version=excluded.algorithm_version,
 pixels=excluded.pixels, commit_seq=excluded.commit_seq, created_at=excluded.created_at`,
		a.RunID, a.ImageID, a.Step, a.Block.X, a.Block.Y, a.Block.W, a.Block.H, a.State,
		a.CacheKey, a.InputHash, a.OutputHash, a.OutputDigest, a.ChainFingerprint,
		a.AlgorithmVersion, compressed, seq, now)
	if err != nil {
		return a, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET updated_at=?, status='running' WHERE run_id=? AND image_id=?`,
		now, a.RunID, a.ImageID); err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	return a, nil
}

func (s *SQLiteStore) CommitBlockManifest(ctx context.Context, m checkpoint.BlockManifest) (checkpoint.BlockManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stepsJSON, err := json.Marshal(m.Steps)
	if err != nil {
		return m, err
	}
	m.ContentHash = m.ComputeContentHash()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return m, err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM (
SELECT MAX(commit_seq) AS seq FROM block_steps
UNION ALL SELECT MAX(commit_seq) AS seq FROM block_manifests)`).Scan(&seq); err != nil {
		return m, err
	}
	m.CommitSeq = seq
	now := time.Now().UnixNano()
	_, err = tx.ExecContext(ctx, `
INSERT INTO block_manifests(run_id,image_id,block_x,block_y,block_w,block_h,
 chain_fingerprint,algorithm_version,steps_json,content_hash,commit_seq,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(run_id,image_id,block_x,block_y) DO UPDATE SET
 block_w=excluded.block_w, block_h=excluded.block_h,
 chain_fingerprint=excluded.chain_fingerprint, algorithm_version=excluded.algorithm_version,
 steps_json=excluded.steps_json, content_hash=excluded.content_hash,
 commit_seq=excluded.commit_seq, created_at=excluded.created_at`,
		m.RunID, m.ImageID, m.Block.X, m.Block.Y, m.Block.W, m.Block.H,
		m.ChainFingerprint, m.AlgorithmVersion, string(stepsJSON), m.ContentHash, seq, now)
	if err != nil {
		return m, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET updated_at=? WHERE run_id=? AND image_id=?`,
		now, m.RunID, m.ImageID); err != nil {
		return m, err
	}
	if err := tx.Commit(); err != nil {
		return m, err
	}
	return m, nil
}

func (s *SQLiteStore) LoadArtifacts(ctx context.Context, runID, imageID string) ([]checkpoint.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id,image_id,step,block_x,block_y,block_w,block_h,state,cache_key,input_hash,
 output_hash,output_digest,chain_fingerprint,algorithm_version,pixels,commit_seq
FROM block_steps WHERE run_id=? AND image_id=? ORDER BY step,block_y,block_x`, runID, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []checkpoint.Artifact
	for rows.Next() {
		var a checkpoint.Artifact
		var compressed []byte
		if err := rows.Scan(&a.RunID, &a.ImageID, &a.Step, &a.Block.X, &a.Block.Y,
			&a.Block.W, &a.Block.H, &a.State, &a.CacheKey, &a.InputHash,
			&a.OutputHash, &a.OutputDigest, &a.ChainFingerprint, &a.AlgorithmVersion,
			&compressed, &a.CommitSeq); err != nil {
			return nil, err
		}
		a.Pixels, err = decompress(compressed)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) LoadBlockManifests(ctx context.Context, runID, imageID string) ([]checkpoint.BlockManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id,image_id,block_x,block_y,block_w,block_h,chain_fingerprint,
 algorithm_version,steps_json,content_hash,commit_seq
FROM block_manifests WHERE run_id=? AND image_id=? ORDER BY block_y,block_x`, runID, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []checkpoint.BlockManifest
	for rows.Next() {
		var m checkpoint.BlockManifest
		var stepsJSON string
		if err := rows.Scan(&m.RunID, &m.ImageID, &m.Block.X, &m.Block.Y, &m.Block.W, &m.Block.H,
			&m.ChainFingerprint, &m.AlgorithmVersion, &stepsJSON, &m.ContentHash, &m.CommitSeq); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(stepsJSON), &m.Steps); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) LookupCache(ctx context.Context, cacheKey, algorithmVersion string) (checkpoint.Artifact, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRowContext(ctx, `
SELECT run_id,image_id,step,block_x,block_y,block_w,block_h,state,cache_key,input_hash,
 output_hash,output_digest,chain_fingerprint,algorithm_version,pixels,commit_seq
FROM block_steps WHERE cache_key=? AND algorithm_version=? AND state='committed'
ORDER BY commit_seq DESC LIMIT 1`, cacheKey, algorithmVersion)
	var a checkpoint.Artifact
	var compressed []byte
	if err := row.Scan(&a.RunID, &a.ImageID, &a.Step, &a.Block.X, &a.Block.Y,
		&a.Block.W, &a.Block.H, &a.State, &a.CacheKey, &a.InputHash,
		&a.OutputHash, &a.OutputDigest, &a.ChainFingerprint, &a.AlgorithmVersion,
		&compressed, &a.CommitSeq); err != nil {
		if err == sql.ErrNoRows {
			return a, false, nil
		}
		return a, false, err
	}
	pixels, err := decompress(compressed)
	if err != nil {
		return a, false, err
	}
	a.Pixels = pixels
	if err := a.Validate(); err != nil {
		return checkpoint.Artifact{}, false, nil
	}
	if checkpoint.PixelHash(pixels) != a.OutputHash {
		return checkpoint.Artifact{}, false, nil
	}
	return a, true, nil
}

func (s *SQLiteStore) InvalidateRun(ctx context.Context, runID, imageID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM block_steps WHERE run_id=? AND image_id=?`,
		runID, imageID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM block_manifests WHERE run_id=? AND image_id=?`,
		runID, imageID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='invalid', updated_at=? WHERE run_id=? AND image_id=?`,
		time.Now().UnixNano(), runID, imageID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_events(run_id,image_id,event,detail,created_at)
VALUES(?,?,?,?,?)`, runID, imageID, "invalidate", reason, time.Now().UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListImages(ctx context.Context, runID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT image_id FROM runs WHERE run_id=? ORDER BY image_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompress(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func HashBytes(b []byte) string {
	return hex.EncodeToString(sha256Sum(b))
}

func sha256Sum(b []byte) []byte {
	h := core.HashParts(string(b))
	raw, _ := hex.DecodeString(h)
	return raw
}
