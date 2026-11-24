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

	_ "modernc.org/sqlite"

	"strata/internal/model"
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

type ChunkCheckpoint struct {
	RunID         string             `json:"runId"`
	Seq           int                `json:"seq"`
	Version       int                `json:"version"`
	ChunkID       string             `json:"chunkId"`
	ParamsHash    string             `json:"paramsHash"`
	ParamsVersion int                `json:"paramsVersion"`
	Algorithm     string             `json:"algorithm"`
	ContentKey    string             `json:"contentKey"`
	ContentHash   string             `json:"contentHash"`
	PointIndices  []int              `json:"pointIndices"`
	Vertices      []model.MeshVertex `json:"vertices"`
	Faces         []model.MeshFace   `json:"faces"`
	Reason        string             `json:"reason"`
	Reused        bool               `json:"reused"`
	CommittedAt   string             `json:"committedAt"`
}

type CachedChunk struct {
	ContentKey   string             `json:"contentKey"`
	ContentHash  string             `json:"contentHash"`
	PointIndices []int              `json:"pointIndices"`
	Vertices     []model.MeshVertex `json:"vertices"`
	Faces        []model.MeshFace   `json:"faces"`
}

type Run struct {
	ID        string `json:"id"`
	Mode      string `json:"mode"`
	Version   int    `json:"version"`
	Status    string `json:"status"`
	MeshHash  string `json:"meshHash"`
	Error     string `json:"error"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type VersionSnapshot struct {
	Version       int             `json:"version"`
	Reason        string          `json:"reason"`
	SnapshotHash  string          `json:"snapshotHash"`
	SourceVersion string          `json:"sourceVersion"`
	PointCount    int             `json:"pointCount"`
	ParamsJSON    json.RawMessage `json:"paramsJson"`
	Points        []model.Point   `json:"points"`
	CreatedAt     string          `json:"createdAt"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "database.path", 0, nil)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "database.open", 0, nil)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, model.NewError(model.EDECODE, err.Error(), "database.pragma", 0, nil)
		}
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  mode TEXT NOT NULL,
  version INTEGER NOT NULL,
  status TEXT NOT NULL,
  mesh_hash TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoints (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  version INTEGER NOT NULL,
  chunk_id TEXT NOT NULL,
  params_hash TEXT NOT NULL,
  params_version INTEGER NOT NULL,
  algorithm TEXT NOT NULL,
  content_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  point_indices BLOB NOT NULL,
  vertices BLOB NOT NULL,
  faces BLOB NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  reused INTEGER NOT NULL DEFAULT 0,
  committed_at TEXT NOT NULL,
  UNIQUE(run_id, chunk_id),
  UNIQUE(run_id, seq)
);
CREATE TABLE IF NOT EXISTS chunk_cache (
  content_key TEXT PRIMARY KEY,
  content_hash TEXT NOT NULL,
  point_indices BLOB NOT NULL,
  vertices BLOB NOT NULL,
  faces BLOB NOT NULL,
  committed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS versions (
  version INTEGER PRIMARY KEY,
  reason TEXT NOT NULL,
  snapshot_hash TEXT NOT NULL,
  source_version TEXT NOT NULL,
  point_count INTEGER NOT NULL,
  params_json BLOB NOT NULL,
  points_json BLOB NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return model.NewError(model.EDECODE, "sqlite migration failed: "+err.Error(), "database.migration", 0, nil)
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) BeginRun(ctx context.Context, id, mode string, version int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(id,mode,version,status,created_at,updated_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status='running',updated_at=excluded.updated_at`,
		id, mode, version, "running", now, now)
	if err != nil {
		return model.NewError(model.ESTATE, "cannot begin checkpoint run: "+err.Error(), "checkpoint.run", 0, nil)
	}
	return nil
}

func (s *Store) FinishRun(ctx context.Context, id, status, meshHash, runErr string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?,mesh_hash=?,error=?,updated_at=? WHERE id=?`,
		status, meshHash, runErr, now, id)
	if err != nil {
		return model.NewError(model.ESTATE, "cannot finish checkpoint run: "+err.Error(), "checkpoint.run", 0, nil)
	}
	return nil
}

func (s *Store) CommitChunk(ctx context.Context, cp ChunkCheckpoint) (ChunkCheckpoint, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	pointsJSON, err := json.Marshal(cp.PointIndices)
	if err != nil {
		return cp, model.NewError(model.EDECODE, err.Error(), "checkpoint.pointIndices", 0, nil)
	}
	verticesJSON, err := json.Marshal(cp.Vertices)
	if err != nil {
		return cp, model.NewError(model.EDECODE, err.Error(), "checkpoint.vertices", 0, nil)
	}
	facesJSON, err := json.Marshal(cp.Faces)
	if err != nil {
		return cp, model.NewError(model.EDECODE, err.Error(), "checkpoint.faces", 0, nil)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return cp, model.NewError(model.ESTATE, err.Error(), "checkpoint.begin", 0, nil)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM checkpoints WHERE run_id=?`, cp.RunID).Scan(&cp.Seq); err != nil {
		return cp, model.NewError(model.ESTATE, err.Error(), "checkpoint.sequence", 0, nil)
	}
	cp.CommittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO checkpoints(
		run_id,seq,version,chunk_id,params_hash,params_version,algorithm,content_key,content_hash,
		point_indices,vertices,faces,reason,reused,committed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		cp.RunID, cp.Seq, cp.Version, cp.ChunkID, cp.ParamsHash, cp.ParamsVersion, cp.Algorithm,
		cp.ContentKey, cp.ContentHash, pointsJSON, verticesJSON, facesJSON, cp.Reason, boolInt(cp.Reused), cp.CommittedAt)
	if err != nil {
		return cp, model.NewError(model.ESTATE, "checkpoint is frozen or already committed: "+err.Error(), "checkpoint.chunk", 0, map[string]any{
			"runId": cp.RunID, "chunkId": cp.ChunkID, "version": cp.Version,
		})
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO chunk_cache(content_key,content_hash,point_indices,vertices,faces,committed_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(content_key) DO UPDATE SET
		content_hash=excluded.content_hash,point_indices=excluded.point_indices,
		vertices=excluded.vertices,faces=excluded.faces,committed_at=excluded.committed_at`,
		cp.ContentKey, cp.ContentHash, pointsJSON, verticesJSON, facesJSON, cp.CommittedAt)
	if err != nil {
		return cp, model.NewError(model.ESTATE, err.Error(), "checkpoint.cache", 0, nil)
	}
	if err := tx.Commit(); err != nil {
		return cp, model.NewError(model.ESTATE, "atomic checkpoint commit failed: "+err.Error(), "checkpoint.commit", 0, nil)
	}
	return cp, nil
}

func (s *Store) LoadCheckpoints(ctx context.Context, runID string) ([]ChunkCheckpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,version,chunk_id,params_hash,params_version,algorithm,
		content_key,content_hash,point_indices,vertices,faces,reason,reused,committed_at
		FROM checkpoints WHERE run_id=? ORDER BY seq`, runID)
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "checkpoint.load", 0, nil)
	}
	defer rows.Close()
	var out []ChunkCheckpoint
	for rows.Next() {
		var cp ChunkCheckpoint
		var pointsJSON, verticesJSON, facesJSON []byte
		var reused int
		if err := rows.Scan(&cp.Seq, &cp.Version, &cp.ChunkID, &cp.ParamsHash, &cp.ParamsVersion,
			&cp.Algorithm, &cp.ContentKey, &cp.ContentHash, &pointsJSON, &verticesJSON, &facesJSON,
			&cp.Reason, &reused, &cp.CommittedAt); err != nil {
			return nil, model.NewError(model.EDECODE, err.Error(), "checkpoint.load", 0, nil)
		}
		cp.RunID = runID
		cp.Reused = reused != 0
		if err := json.Unmarshal(pointsJSON, &cp.PointIndices); err != nil {
			return nil, model.NewError(model.EDECODE, "invalid point index checkpoint", "checkpoint.pointIndices", 0, nil)
		}
		if err := json.Unmarshal(verticesJSON, &cp.Vertices); err != nil {
			return nil, model.NewError(model.EDECODE, "invalid vertex checkpoint", "checkpoint.vertices", 0, nil)
		}
		if err := json.Unmarshal(facesJSON, &cp.Faces); err != nil {
			return nil, model.NewError(model.EDECODE, "invalid face checkpoint", "checkpoint.faces", 0, nil)
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

func (s *Store) LoadRun(ctx context.Context, runID string) (*Run, error) {
	var r Run
	err := s.db.QueryRowContext(ctx, `SELECT id,mode,version,status,mesh_hash,error,created_at,updated_at FROM runs WHERE id=?`, runID).
		Scan(&r.ID, &r.Mode, &r.Version, &r.Status, &r.MeshHash, &r.Error, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, model.NewError(model.ESTATE, "checkpoint run not found", "checkpoint.run", 0, map[string]any{"runId": runID})
	}
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "checkpoint.run", 0, nil)
	}
	return &r, nil
}

func (s *Store) LatestRun(ctx context.Context, mode string) (*Run, error) {
	var r Run
	err := s.db.QueryRowContext(ctx, `SELECT id,mode,version,status,mesh_hash,error,created_at,updated_at
		FROM runs WHERE mode=? ORDER BY created_at DESC LIMIT 1`, mode).
		Scan(&r.ID, &r.Mode, &r.Version, &r.Status, &r.MeshHash, &r.Error, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "checkpoint.run", 0, nil)
	}
	return &r, nil
}

func (s *Store) LoadCachedChunk(ctx context.Context, contentKey string) (*CachedChunk, error) {
	var c CachedChunk
	var pointsJSON, verticesJSON, facesJSON []byte
	err := s.db.QueryRowContext(ctx, `SELECT content_key,content_hash,point_indices,vertices,faces
		FROM chunk_cache WHERE content_key=?`, contentKey).
		Scan(&c.ContentKey, &c.ContentHash, &pointsJSON, &verticesJSON, &facesJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "cache.load", 0, nil)
	}
	if err := json.Unmarshal(pointsJSON, &c.PointIndices); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "cache.pointIndices", 0, nil)
	}
	if err := json.Unmarshal(verticesJSON, &c.Vertices); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "cache.vertices", 0, nil)
	}
	if err := json.Unmarshal(facesJSON, &c.Faces); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "cache.faces", 0, nil)
	}
	return &c, nil
}

func (s *Store) SaveVersion(ctx context.Context, v VersionSnapshot) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	v.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	pointsJSON, err := json.Marshal(v.Points)
	if err != nil {
		return model.NewError(model.EDECODE, err.Error(), "version.points", 0, nil)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO versions(version,reason,snapshot_hash,source_version,point_count,params_json,points_json,created_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(version) DO UPDATE SET reason=excluded.reason,
		snapshot_hash=excluded.snapshot_hash,source_version=excluded.source_version,point_count=excluded.point_count,
		params_json=excluded.params_json,points_json=excluded.points_json,created_at=excluded.created_at`,
		v.Version, v.Reason, v.SnapshotHash, v.SourceVersion, v.PointCount, []byte(v.ParamsJSON), pointsJSON, v.CreatedAt)
	if err != nil {
		return model.NewError(model.ESTATE, "cannot snapshot version: "+err.Error(), "version", 0, nil)
	}
	return nil
}

func (s *Store) LoadVersion(ctx context.Context, version int) (*VersionSnapshot, error) {
	var v VersionSnapshot
	var paramsJSON, pointsJSON []byte
	err := s.db.QueryRowContext(ctx, `SELECT version,reason,snapshot_hash,source_version,point_count,params_json,points_json,created_at
		FROM versions WHERE version=?`, version).
		Scan(&v.Version, &v.Reason, &v.SnapshotHash, &v.SourceVersion, &v.PointCount, &paramsJSON, &pointsJSON, &v.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, model.NewError(model.ESTATE, "version snapshot not found", "version", 0, map[string]any{"version": version})
	}
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "version", 0, nil)
	}
	v.ParamsJSON = paramsJSON
	if err := json.Unmarshal(pointsJSON, &v.Points); err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "version.points", 0, nil)
	}
	return &v, nil
}

func (s *Store) LatestVersion(ctx context.Context) (*VersionSnapshot, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM versions`).Scan(&n)
	if err != nil {
		return nil, model.NewError(model.EDECODE, err.Error(), "version", 0, nil)
	}
	if n == 0 {
		return nil, nil
	}
	return s.LoadVersion(ctx, n)
}

func (s *Store) CheckpointCount(ctx context.Context, runID string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints WHERE run_id=?`, runID).Scan(&n); err != nil {
		return 0, model.NewError(model.EDECODE, err.Error(), "checkpoint.count", 0, nil)
	}
	return n, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) String() string {
	return fmt.Sprintf("sqlite:%p", s.db)
}
