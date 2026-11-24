package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cadpipe/internal/cad"
	"cadpipe/internal/render"

	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
	path    string
}

type ExpectedTile struct {
	TileID           int
	DependencyHash   string
	SceneVersion     int64
	CameraVersion    int64
	AlgorithmVersion string
}

type SnapshotMismatchError struct {
	Reason string
	TileID int
}

func (e *SnapshotMismatchError) Error() string {
	return fmt.Sprintf("snapshot invalid and fully discarded: %s (tile=%d)", e.Reason, e.TileID)
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
	pragmas := []string{
		"PRAGMA journal_mode=DELETE",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pragma %s: %w", p, err)
		}
	}
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS checkpoints (
  tile_id INTEGER PRIMARY KEY,
  tile_order INTEGER NOT NULL,
  tile_x INTEGER NOT NULL,
  tile_y INTEGER NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  color_digest TEXT NOT NULL,
  depth_digest TEXT NOT NULL,
  color_blob BLOB NOT NULL,
  depth_blob BLOB NOT NULL,
  coverage REAL NOT NULL,
  outline_coverage REAL NOT NULL,
  patch_coverage_json TEXT NOT NULL,
  light_stats_json TEXT NOT NULL,
  scene_structure_version INTEGER NOT NULL,
  camera_version INTEGER NOT NULL,
  algorithm_version TEXT NOT NULL,
  dependency_hash TEXT NOT NULL DEFAULT '',
  committed_at_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_order ON checkpoints(tile_order);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

// Save commits one complete tile in its own immediate transaction. The mutex
// guarantees that writers never interleave; the pipeline calls tiles in
// canonical tile_order.
func (s *Store) Save(ctx context.Context, tile *render.TileResult) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	depthBlob := make([]byte, 8*len(tile.Depth))
	for i, d := range tile.Depth {
		binary.BigEndian.PutUint64(depthBlob[i*8:i*8+8], math.Float64bits(d))
	}
	patchJSON, err := json.Marshal(tile.PatchCoverage)
	if err != nil {
		return err
	}
	statsJSON, err := json.Marshal(tile.LightStats)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
INSERT INTO checkpoints (
  tile_id,tile_order,tile_x,tile_y,width,height,cache_key,content_hash,
  color_digest,depth_digest,color_blob,depth_blob,coverage,outline_coverage,
  patch_coverage_json,light_stats_json,scene_structure_version,camera_version,
  algorithm_version,dependency_hash,committed_at_ns
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(tile_id) DO UPDATE SET
  tile_order=excluded.tile_order,tile_x=excluded.tile_x,tile_y=excluded.tile_y,
  width=excluded.width,height=excluded.height,cache_key=excluded.cache_key,
  content_hash=excluded.content_hash,color_digest=excluded.color_digest,
  depth_digest=excluded.depth_digest,color_blob=excluded.color_blob,
  depth_blob=excluded.depth_blob,coverage=excluded.coverage,
  outline_coverage=excluded.outline_coverage,
  patch_coverage_json=excluded.patch_coverage_json,
  light_stats_json=excluded.light_stats_json,
  scene_structure_version=excluded.scene_structure_version,
  camera_version=excluded.camera_version,algorithm_version=excluded.algorithm_version,
  dependency_hash=excluded.dependency_hash,
  committed_at_ns=excluded.committed_at_ns`,
		tile.TileID, tile.TileID, tile.X, tile.Y, tile.Width, tile.Height,
		tile.CacheKey, tile.ContentHash, tile.ColorDigest, tile.DepthDigest,
		tile.Color, depthBlob, tile.Coverage, tile.OutlineCoverage,
		string(patchJSON), string(statsJSON), tile.SceneVersion, tile.CameraVersion,
		tile.AlgorithmVersion, tile.DependencyHash, time.Now().UnixNano())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM checkpoints").Scan(&n)
	return n, err
}

func (s *Store) Clear(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM checkpoints"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM meta"); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadCompatible returns only when every persisted row is internally intact
// and matches the current versioned expectation. Any mismatch purges the whole
// checkpoint table before returning SnapshotMismatchError.
func (s *Store) LoadCompatible(ctx context.Context, expected map[int]ExpectedTile) (map[int]*render.TileResult, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT tile_id,tile_x,tile_y,width,height,cache_key,content_hash,color_digest,
       depth_digest,color_blob,depth_blob,coverage,outline_coverage,
       patch_coverage_json,light_stats_json,scene_structure_version,
       camera_version,algorithm_version,dependency_hash
FROM checkpoints ORDER BY tile_order`)
	if err != nil {
		return nil, err
	}
	type row struct {
		tile                                            int
		x, y, w, h                                      int
		cacheKey, contentHash, colorDigest, depthDigest string
		colorBlob, depthBlob                            []byte
		coverage, outline                               float64
		patchJSON, statsJSON                            string
		sceneVersion, cameraVersion                     int64
		algorithm                                       string
		dependencyHash                                  string
	}
	var rowsData []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.tile, &r.x, &r.y, &r.w, &r.h, &r.cacheKey, &r.contentHash,
			&r.colorDigest, &r.depthDigest, &r.colorBlob, &r.depthBlob, &r.coverage,
			&r.outline, &r.patchJSON, &r.statsJSON, &r.sceneVersion, &r.cameraVersion,
			&r.algorithm, &r.dependencyHash); err != nil {
			_ = rows.Close()
			return nil, err
		}
		rowsData = append(rowsData, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(rowsData) == 0 {
		return map[int]*render.TileResult{}, nil
	}

	for _, r := range rowsData {
		exp, ok := expected[r.tile]
		if !ok {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "persisted tile not in current grid", TileID: r.tile})
		}
		if r.sceneVersion != exp.SceneVersion {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "scene structure version mismatch", TileID: r.tile})
		}
		if r.cameraVersion != exp.CameraVersion {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "camera version mismatch", TileID: r.tile})
		}
		if r.algorithm != exp.AlgorithmVersion {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "algorithm version mismatch", TileID: r.tile})
		}
		if r.dependencyHash != exp.DependencyHash {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "semantic dependency hash mismatch", TileID: r.tile})
		}
		if expectedKey := render.BuildCacheKey(r.tile, r.contentHash, r.sceneVersion, r.cameraVersion); r.cacheKey != expectedKey {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "cache key does not match content/version identity", TileID: r.tile})
		}
		if got := cad.SHA256Hex(r.colorBlob); got != r.colorDigest {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "color digest mismatch", TileID: r.tile})
		}
		depth, err := decodeDepth(r.depthBlob)
		if err != nil {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: err.Error(), TileID: r.tile})
		}
		if got := render.DepthDigest(depth); got != r.depthDigest {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "depth digest mismatch", TileID: r.tile})
		}
	}

	out := make(map[int]*render.TileResult, len(rowsData))
	for _, r := range rowsData {
		depth, err := decodeDepth(r.depthBlob)
		if err != nil {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: err.Error(), TileID: r.tile})
		}
		var patchCoverage map[string]float64
		if err := json.Unmarshal([]byte(r.patchJSON), &patchCoverage); err != nil {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "patch coverage JSON corrupt", TileID: r.tile})
		}
		var stats render.LightStats
		if err := json.Unmarshal([]byte(r.statsJSON), &stats); err != nil {
			return nil, s.invalidate(ctx, &SnapshotMismatchError{Reason: "light stats JSON corrupt", TileID: r.tile})
		}
		color := append([]byte(nil), r.colorBlob...)
		out[r.tile] = &render.TileResult{
			TileID: r.tile, X: r.x, Y: r.y, Width: r.w, Height: r.h,
			Color: color, Depth: depth, ColorDigest: r.colorDigest, DepthDigest: r.depthDigest,
			ContentHash: r.contentHash, CacheKey: r.cacheKey, Coverage: r.coverage,
			OutlineCoverage: r.outline, PatchCoverage: patchCoverage, LightStats: stats,
			SceneVersion: r.sceneVersion, CameraVersion: r.cameraVersion, AlgorithmVersion: r.algorithm,
		}
	}
	return out, nil
}

func (s *Store) invalidate(ctx context.Context, cause *SnapshotMismatchError) error {
	_ = s.Clear(ctx)
	return cause
}

func decodeDepth(blob []byte) ([]float64, error) {
	if len(blob)%8 != 0 {
		return nil, fmt.Errorf("depth blob length %d is not 8-byte aligned", len(blob))
	}
	out := make([]float64, len(blob)/8)
	for i := range out {
		bits := binary.BigEndian.Uint64(blob[i*8 : i*8+8])
		out[i] = math.Float64frombits(bits)
	}
	return out, nil
}
