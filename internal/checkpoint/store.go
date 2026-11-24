package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"fixedsampler/internal/engine"
	"fixedsampler/internal/model"
)

var ErrSnapshotInvalid = errors.New("checkpoint snapshot invalid")

type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	path string
}

type row struct {
	ProjectID     string
	ChunkIndex    int
	ChunkX        int
	ChunkY        int
	ConfigFP      string
	ParamVersion  string
	AlgoVersion   string
	ArtifactJSON  string
	CacheKeysJSON string
	ContentJSON   string
	InputJSON     string
	UpstreamJSON  string
	FinalDigest   string
	CommitOrder   int
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS snapshots (
  project_id TEXT NOT NULL,
  config_fingerprint TEXT NOT NULL,
  status TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  chunk_count INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(project_id, config_fingerprint)
);
CREATE TABLE IF NOT EXISTS chunk_checkpoints (
  project_id TEXT NOT NULL,
  chunk_index INTEGER NOT NULL,
  chunk_x INTEGER NOT NULL,
  chunk_y INTEGER NOT NULL,
  config_fingerprint TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  artifact_json TEXT NOT NULL,
  cache_keys_json TEXT NOT NULL,
  content_hashes_json TEXT NOT NULL,
  input_hashes_json TEXT NOT NULL,
  upstream_keys_json TEXT NOT NULL,
  final_digest TEXT NOT NULL,
  commit_order INTEGER NOT NULL,
  committed_at TEXT NOT NULL,
  PRIMARY KEY(project_id, chunk_index)
);
CREATE TABLE IF NOT EXISTS node_cache (
  cache_key TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  stage_seq INTEGER NOT NULL,
  chunk_index INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  artifact_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_node_cache_project ON node_cache(project_id);
`)
	return err
}

func (s *Store) CommitChunk(ctx context.Context, c *engine.ChunkCommit) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	artifactJSON, err := json.Marshal(c.Chunk)
	if err != nil {
		return err
	}
	cacheJSON, _ := json.Marshal(c.CacheKeys)
	contentJSON, _ := json.Marshal(c.ContentHashes)
	inputJSON, _ := json.Marshal(c.InputHashes)
	upstreamJSON, _ := json.Marshal(c.UpstreamKeys)
	finalDigest := ""
	if len(c.ContentHashes) > 0 {
		finalDigest = c.ContentHashes[len(c.ContentHashes)-1]
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
INSERT INTO chunk_checkpoints(
 project_id,chunk_index,chunk_x,chunk_y,config_fingerprint,param_version,algo_version,
 artifact_json,cache_keys_json,content_hashes_json,input_hashes_json,upstream_keys_json,
 final_digest,commit_order,committed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(project_id,chunk_index) DO UPDATE SET
 chunk_x=excluded.chunk_x, chunk_y=excluded.chunk_y,
 config_fingerprint=excluded.config_fingerprint,
 param_version=excluded.param_version, algo_version=excluded.algo_version,
 artifact_json=excluded.artifact_json, cache_keys_json=excluded.cache_keys_json,
 content_hashes_json=excluded.content_hashes_json, input_hashes_json=excluded.input_hashes_json,
 upstream_keys_json=excluded.upstream_keys_json, final_digest=excluded.final_digest,
 commit_order=excluded.commit_order, committed_at=excluded.committed_at`,
		c.Chunk.ProjectID, c.Chunk.ChunkIndex, c.Chunk.ChunkX, c.Chunk.ChunkY, c.ConfigFP,
		model.ParamVersion, model.AlgoVersion, string(artifactJSON), string(cacheJSON),
		string(contentJSON), string(inputJSON), string(upstreamJSON), finalDigest,
		c.Chunk.ChunkIndex+1, now,
	)
	if err != nil {
		return err
	}

	for seq, key := range c.CacheKeys {
		var level model.LevelArtifact
		if seq < len(c.Chunk.Levels) {
			level = c.Chunk.Levels[seq]
		}
		levelJSON, err := json.Marshal(level)
		if err != nil {
			return err
		}
		contentHash := ""
		if seq < len(c.ContentHashes) {
			contentHash = c.ContentHashes[seq]
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO node_cache(cache_key,project_id,stage_seq,chunk_index,content_hash,param_version,algo_version,artifact_json)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(cache_key) DO UPDATE SET content_hash=excluded.content_hash, artifact_json=excluded.artifact_json`,
			key, c.Chunk.ProjectID, seq, c.Chunk.ChunkIndex, contentHash,
			model.ParamVersion, model.AlgoVersion, string(levelJSON))
		if err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO snapshots(project_id,config_fingerprint,status,param_version,algo_version,chunk_count,updated_at)
VALUES(?,?, 'open', ?, ?, ?, ?)
ON CONFLICT(project_id,config_fingerprint) DO UPDATE SET
 status='open', param_version=excluded.param_version, algo_version=excluded.algo_version,
 chunk_count=excluded.chunk_count, updated_at=excluded.updated_at`,
		c.Chunk.ProjectID, c.ConfigFP, model.ParamVersion, model.AlgoVersion, 0, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkComplete(ctx context.Context, projectID, configFP string, chunkCount int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO snapshots(project_id,config_fingerprint,status,param_version,algo_version,chunk_count,updated_at)
VALUES(?,?, 'complete', ?, ?, ?, ?)
ON CONFLICT(project_id,config_fingerprint) DO UPDATE SET status='complete', chunk_count=excluded.chunk_count, updated_at=excluded.updated_at`,
		projectID, configFP, model.ParamVersion, model.AlgoVersion, chunkCount,
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) SaveProjectMeta(ctx context.Context, p *model.Project, configFP string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		"project:"+p.ProjectID, string(data))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		"config_fp:"+p.ProjectID, configFP)
	return err
}

func (s *Store) ResetProject(ctx context.Context, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM chunk_checkpoints WHERE project_id=?`,
		`DELETE FROM node_cache WHERE project_id=?`,
		`DELETE FROM snapshots WHERE project_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, projectID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadPrevious(ctx context.Context, projectID string) (map[int]*engine.PreviousChunk, error) {
	rows, err := s.readRows(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := map[int]*engine.PreviousChunk{}
	for _, r := range rows {
		if r.ParamVersion != model.ParamVersion || r.AlgoVersion != model.AlgoVersion {
			_ = s.ResetProject(ctx, projectID)
			return nil, fmt.Errorf("%w: version mismatch param=%s algo=%s", ErrSnapshotInvalid, r.ParamVersion, r.AlgoVersion)
		}
		var chunk model.ChunkArtifact
		if err := json.Unmarshal([]byte(r.ArtifactJSON), &chunk); err != nil {
			_ = s.ResetProject(ctx, projectID)
			return nil, fmt.Errorf("%w: artifact decode: %v", ErrSnapshotInvalid, err)
		}
		var contentHashes []string
		if err := json.Unmarshal([]byte(r.ContentJSON), &contentHashes); err != nil {
			_ = s.ResetProject(ctx, projectID)
			return nil, fmt.Errorf("%w: content hashes decode: %v", ErrSnapshotInvalid, err)
		}
		for seq, level := range chunk.Levels {
			if seq >= len(contentHashes) {
				_ = s.ResetProject(ctx, projectID)
				return nil, fmt.Errorf("%w: missing content hash seq=%d chunk=%d", ErrSnapshotInvalid, seq, chunk.ChunkIndex)
			}
			if got := engine.HashTiles(level.Tiles); got != contentHashes[seq] {
				_ = s.ResetProject(ctx, projectID)
				return nil, fmt.Errorf("%w: content hash mismatch chunk=%d seq=%d", ErrSnapshotInvalid, chunk.ChunkIndex, seq)
			}
		}
		prev := &engine.PreviousChunk{Artifact: &chunk, ConfigFP: r.ConfigFP, ContentHashes: contentHashes}
		_ = json.Unmarshal([]byte(r.CacheKeysJSON), &prev.CacheKeys)
		_ = json.Unmarshal([]byte(r.InputJSON), &prev.InputHashes)
		_ = json.Unmarshal([]byte(r.UpstreamJSON), &prev.UpstreamKeys)
		out[chunk.ChunkIndex] = prev
	}
	return out, nil
}

func (s *Store) LastConfigFP(ctx context.Context, projectID string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, "config_fp:"+projectID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) LoadSameConfig(ctx context.Context, projectID, expectedFP string) (map[int]*engine.PreviousChunk, error) {
	previous, err := s.LoadPrevious(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for chunkIndex, chunk := range previous {
		if chunk.ConfigFP != expectedFP {
			_ = s.ResetProject(ctx, projectID)
			return nil, fmt.Errorf("%w: chunk %d config fingerprint mismatch: row=%s current=%s",
				ErrSnapshotInvalid, chunkIndex, chunk.ConfigFP, expectedFP)
		}
	}
	return previous, nil
}

func (s *Store) LoadArtifacts(ctx context.Context, projectID string) (map[int]*model.ChunkArtifact, []int, error) {
	rows, err := s.readRows(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	out := map[int]*model.ChunkArtifact{}
	seq := make([]int, 0, len(rows))
	for _, r := range rows {
		var chunk model.ChunkArtifact
		if err := json.Unmarshal([]byte(r.ArtifactJSON), &chunk); err != nil {
			return nil, nil, err
		}
		out[chunk.ChunkIndex] = &chunk
		seq = append(seq, chunk.ChunkIndex)
	}
	return out, seq, nil
}

func (s *Store) readRows(ctx context.Context, projectID string) ([]row, error) {
	rs, err := s.db.QueryContext(ctx, `
SELECT project_id,chunk_index,chunk_x,chunk_y,config_fingerprint,param_version,algo_version,
       artifact_json,cache_keys_json,content_hashes_json,input_hashes_json,upstream_keys_json,
       final_digest,commit_order
FROM chunk_checkpoints WHERE project_id=? ORDER BY commit_order,chunk_index`, projectID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var rows []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.ProjectID, &r.ChunkIndex, &r.ChunkX, &r.ChunkY, &r.ConfigFP,
			&r.ParamVersion, &r.AlgoVersion, &r.ArtifactJSON, &r.CacheKeysJSON,
			&r.ContentJSON, &r.InputJSON, &r.UpstreamJSON, &r.FinalDigest, &r.CommitOrder); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, rs.Err()
}

func (s *Store) TamperContentHash(ctx context.Context, projectID string, chunkIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE chunk_checkpoints SET content_hashes_json=? WHERE project_id=? AND chunk_index=?`,
		`["tampered"]`, projectID, chunkIndex)
	return err
}

func (s *Store) TamperVersion(ctx context.Context, projectID string, chunkIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE chunk_checkpoints SET algo_version='algo-bad' WHERE project_id=? AND chunk_index=?`,
		projectID, chunkIndex)
	return err
}
