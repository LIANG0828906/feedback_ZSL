package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"maskpipe/internal/model"

	_ "modernc.org/sqlite"
)

type SnapshotInfo struct {
	ID               string `json:"id"`
	SceneID          string `json:"scene_id"`
	AlgorithmVersion string `json:"algorithm_version"`
	ParamVersion     string `json:"param_version"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	TileWidth        int    `json:"tile_width"`
	TileHeight       int    `json:"tile_height"`
	Seed             int64  `json:"seed"`
	TruthFingerprint string `json:"truth_fingerprint"`
	ParamsJSON       string `json:"params_json,omitempty"`
}

type StageRecord struct {
	SnapshotID     string      `json:"snapshot_id"`
	ChunkX         int         `json:"chunk_x"`
	ChunkY         int         `json:"chunk_y"`
	Stage          model.Stage `json:"stage"`
	CanonicalOrder int         `json:"canonical_order"`
	ParamVersion   string      `json:"param_version"`
	AlgoVersion    string      `json:"algo_version"`
	Downsample     float64     `json:"downsample"`
	SummaryJSON    string      `json:"summary_json"`
	GeometryJSON   string      `json:"geometry_json,omitempty"`
	Blob           []byte      `json:"-"`
	ContentHash    string      `json:"content_hash"`
	WriteOrder     int64       `json:"write_order"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
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
	s := &Store{db: db}
	if err := s.initSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS snapshot_meta (
  snapshot_id TEXT NOT NULL,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  PRIMARY KEY(snapshot_id, key)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS stages (
  write_order INTEGER PRIMARY KEY AUTOINCREMENT,
  snapshot_id TEXT NOT NULL,
  chunk_x INTEGER NOT NULL,
  chunk_y INTEGER NOT NULL,
  stage TEXT NOT NULL,
  canonical_order INTEGER NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  downsample REAL NOT NULL,
  summary_json TEXT NOT NULL,
  geometry_json TEXT NOT NULL DEFAULT '',
  blob BLOB NOT NULL,
  content_hash TEXT NOT NULL,
  UNIQUE(snapshot_id, chunk_x, chunk_y, stage)
);
CREATE INDEX IF NOT EXISTS idx_stages_snapshot_order ON stages(snapshot_id, canonical_order);
`)
	return err
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HashJSON(v any) (string, string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", "", err
	}
	return string(b), HashBytes(b), nil
}

func (s *Store) InitSnapshot(ctx context.Context, info SnapshotInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	values := map[string]any{
		"scene_id": info.SceneID, "algo_version": info.AlgorithmVersion,
		"param_version": info.ParamVersion, "width": info.Width,
		"height": info.Height, "tile_width": info.TileWidth,
		"tile_height": info.TileHeight, "seed": info.Seed,
		"truth_fingerprint": info.TruthFingerprint,
		"params_json":       info.ParamsJSON,
	}
	for k, v := range values {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO snapshot_meta(snapshot_id,key,value) VALUES(?,?,?)
			 ON CONFLICT(snapshot_id,key) DO NOTHING`,
			info.ID, k, fmt.Sprint(v)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Meta(ctx context.Context, snapshotID, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM snapshot_meta WHERE snapshot_id=? AND key=?`,
		snapshotID, key).Scan(&v)
	return v, err
}

func (s *Store) SnapshotInfo(ctx context.Context, id string) (SnapshotInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM snapshot_meta WHERE snapshot_id=?`, id)
	if err != nil {
		return SnapshotInfo{}, err
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return SnapshotInfo{}, err
		}
		values[k] = v
	}
	atoi := func(k string) int { var n int; fmt.Sscan(values[k], &n); return n }
	var seed int64
	fmt.Sscan(values["seed"], &seed)
	return SnapshotInfo{
		ID: id, SceneID: values["scene_id"], AlgorithmVersion: values["algo_version"],
		ParamVersion: values["param_version"], Width: atoi("width"), Height: atoi("height"),
		TileWidth: atoi("tile_width"), TileHeight: atoi("tile_height"), Seed: seed,
		TruthFingerprint: values["truth_fingerprint"], ParamsJSON: values["params_json"],
	}, rows.Err()
}

// CommitStage serializes all SQLite transactions. The single connection plus
// this mutex guarantees that stage writes never interleave.
func (s *Store) CommitStage(ctx context.Context, rec StageRecord) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
INSERT INTO stages(snapshot_id,chunk_x,chunk_y,stage,canonical_order,param_version,
  algo_version,downsample,summary_json,geometry_json,blob,content_hash)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(snapshot_id,chunk_x,chunk_y,stage) DO NOTHING`,
		rec.SnapshotID, rec.ChunkX, rec.ChunkY, string(rec.Stage), rec.CanonicalOrder,
		rec.ParamVersion, rec.AlgoVersion, rec.Downsample, rec.SummaryJSON,
		rec.GeometryJSON, rec.Blob, rec.ContentHash)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (s *Store) HasStage(ctx context.Context, snapshotID string, x, y int, stage model.Stage) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM stages WHERE snapshot_id=? AND chunk_x=? AND chunk_y=? AND stage=?`,
		snapshotID, x, y, string(stage)).Scan(&n)
	return n > 0, err
}

func (s *Store) GetStage(ctx context.Context, snapshotID string, x, y int, stage model.Stage) (StageRecord, error) {
	var rec StageRecord
	var stageName string
	err := s.db.QueryRowContext(ctx, `
SELECT write_order,snapshot_id,chunk_x,chunk_y,stage,canonical_order,param_version,
       algo_version,downsample,summary_json,geometry_json,blob,content_hash
FROM stages WHERE snapshot_id=? AND chunk_x=? AND chunk_y=? AND stage=?`,
		snapshotID, x, y, string(stage)).Scan(
		&rec.WriteOrder, &rec.SnapshotID, &rec.ChunkX, &rec.ChunkY, &stageName,
		&rec.CanonicalOrder, &rec.ParamVersion, &rec.AlgoVersion, &rec.Downsample,
		&rec.SummaryJSON, &rec.GeometryJSON, &rec.Blob, &rec.ContentHash)
	rec.Stage = model.Stage(stageName)
	return rec, err
}

func (s *Store) ListStages(ctx context.Context, snapshotID string) ([]StageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT write_order,snapshot_id,chunk_x,chunk_y,stage,canonical_order,param_version,
       algo_version,downsample,summary_json,geometry_json,blob,content_hash
FROM stages WHERE snapshot_id=? ORDER BY canonical_order ASC`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StageRecord
	for rows.Next() {
		var rec StageRecord
		var stageName string
		if err := rows.Scan(
			&rec.WriteOrder, &rec.SnapshotID, &rec.ChunkX, &rec.ChunkY, &stageName,
			&rec.CanonicalOrder, &rec.ParamVersion, &rec.AlgoVersion, &rec.Downsample,
			&rec.SummaryJSON, &rec.GeometryJSON, &rec.Blob, &rec.ContentHash); err != nil {
			return nil, err
		}
		rec.Stage = model.Stage(stageName)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) PurgeSnapshot(ctx context.Context, snapshotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM stages WHERE snapshot_id=?`, snapshotID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_meta WHERE snapshot_id=?`, snapshotID); err != nil {
		return err
	}
	return tx.Commit()
}
