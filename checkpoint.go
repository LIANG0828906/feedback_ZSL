package fog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type CheckpointStore struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func OpenCheckpointStore(path string) (*CheckpointStore, error) {
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
	store := &CheckpointStore{db: db, path: path}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *CheckpointStore) Path() string { return s.path }

func (s *CheckpointStore) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS tile_checkpoints (
  tile_x INTEGER NOT NULL,
  tile_y INTEGER NOT NULL,
  commit_order INTEGER NOT NULL,
  status TEXT NOT NULL,
  ray_segment_digest TEXT NOT NULL,
  transmittance REAL NOT NULL,
  scattering REAL NOT NULL,
  average_scattering REAL NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  recipe_version INTEGER NOT NULL,
  camera_version INTEGER NOT NULL,
  algo_version TEXT NOT NULL,
  pixel_json TEXT NOT NULL,
  tile_json TEXT NOT NULL,
  committed_at TEXT NOT NULL,
  PRIMARY KEY(tile_x, tile_y)
);
CREATE TABLE IF NOT EXISTS commit_journal (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  tile_x INTEGER NOT NULL,
  tile_y INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  committed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_commit_journal_tile ON commit_journal(tile_x, tile_y, seq);
`)
	return err
}

func (s *CheckpointStore) CommitTile(ctx context.Context, tile TileResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tileJSON, err := json.Marshal(tile)
	if err != nil {
		return err
	}
	pixelJSON, err := json.Marshal(tile.Pixels)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
INSERT INTO tile_checkpoints (
 tile_x,tile_y,commit_order,status,ray_segment_digest,transmittance,scattering,average_scattering,
 cache_key,content_hash,recipe_version,camera_version,algo_version,pixel_json,tile_json,committed_at
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(tile_x,tile_y) DO UPDATE SET
 commit_order=excluded.commit_order,
 status=excluded.status,
 ray_segment_digest=excluded.ray_segment_digest,
 transmittance=excluded.transmittance,
 scattering=excluded.scattering,
 average_scattering=excluded.average_scattering,
 cache_key=excluded.cache_key,
 content_hash=excluded.content_hash,
 recipe_version=excluded.recipe_version,
 camera_version=excluded.camera_version,
 algo_version=excluded.algo_version,
 pixel_json=excluded.pixel_json,
 tile_json=excluded.tile_json,
 committed_at=excluded.committed_at`,
		tile.Tile.X, tile.Tile.Y, tile.CommitOrder, tile.Status, tile.RaySegmentDigest,
		tile.Transmittance, tile.Scattering, tile.AverageScattering, tile.CacheKey, tile.ContentHash,
		tile.RecipeVersion, tile.CameraVersion, tile.AlgoVersion, string(pixelJSON), string(tileJSON), now,
	)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO commit_journal(tile_x,tile_y,cache_key,committed_at) VALUES (?,?,?,?)`,
		tile.Tile.X, tile.Tile.Y, tile.CacheKey, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type StaleCheckpoint struct {
	Tile     TileCoord `json:"tile"`
	Field    string    `json:"field"`
	Stored   any       `json:"stored"`
	Expected any       `json:"expected"`
	Reason   string    `json:"reason"`
}

func (s *CheckpointStore) LoadValidTile(ctx context.Context, scene *Scene, tile TileCoord) (*TileResult, *StaleCheckpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var tileJSON, contentHash, cacheKey, algoVersion string
	var recipeVersion, cameraVersion int64
	var status string
	err := s.db.QueryRowContext(ctx, `
SELECT status,cache_key,content_hash,recipe_version,camera_version,algo_version,tile_json
FROM tile_checkpoints WHERE tile_x=? AND tile_y=?`, tile.X, tile.Y).Scan(
		&status, &cacheKey, &contentHash, &recipeVersion, &cameraVersion, &algoVersion, &tileJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	expectedHash := TileContentHashForCoord(scene, tile)
	if contentHash != expectedHash {
		stale := &StaleCheckpoint{Tile: tile, Field: "content_hash", Stored: contentHash, Expected: expectedHash, Reason: "medium/light content hash mismatch"}
		return nil, stale, s.invalidateLocked(ctx, tile)
	}
	if recipeVersion != scene.Recipe.Version {
		stale := &StaleCheckpoint{Tile: tile, Field: "recipe_version", Stored: recipeVersion, Expected: scene.Recipe.Version, Reason: "recipe version mismatch"}
		return nil, stale, s.invalidateLocked(ctx, tile)
	}
	if algoVersion != AlgoVersion {
		stale := &StaleCheckpoint{Tile: tile, Field: "algo_version", Stored: algoVersion, Expected: AlgoVersion, Reason: "algorithm version mismatch"}
		return nil, stale, s.invalidateLocked(ctx, tile)
	}
	if cameraVersion != scene.CameraTrack.Version {
		stale := &StaleCheckpoint{Tile: tile, Field: "camera_version", Stored: cameraVersion, Expected: scene.CameraTrack.Version, Reason: "camera version mismatch"}
		return nil, stale, s.invalidateLocked(ctx, tile)
	}

	var result TileResult
	if err := json.Unmarshal([]byte(tileJSON), &result); err != nil {
		stale := &StaleCheckpoint{Tile: tile, Field: "tile_json", Stored: "corrupt-json", Expected: "valid-json", Reason: err.Error()}
		return nil, stale, s.invalidateLocked(ctx, tile)
	}
	if result.CacheKey != cacheKey {
		stale := &StaleCheckpoint{Tile: tile, Field: "cache_key", Stored: cacheKey, Expected: result.CacheKey, Reason: "cache key column does not match payload"}
		return nil, stale, s.invalidateLocked(ctx, tile)
	}
	return &result, nil, nil
}

func (s *CheckpointStore) invalidateLocked(ctx context.Context, tile TileCoord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM tile_checkpoints WHERE tile_x=? AND tile_y=?`, tile.X, tile.Y); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commit_journal WHERE tile_x=? AND tile_y=?`, tile.X, tile.Y); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CheckpointStore) CommitSequence(ctx context.Context) ([]TileCoord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tile_x,tile_y FROM commit_journal ORDER BY seq ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TileCoord{}
	for rows.Next() {
		var tile TileCoord
		if err := rows.Scan(&tile.X, &tile.Y); err != nil {
			return nil, err
		}
		out = append(out, tile)
	}
	return out, rows.Err()
}

func (s *CheckpointStore) CheckpointSequence(ctx context.Context) ([]TileCoord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tile_x,tile_y FROM tile_checkpoints ORDER BY commit_order ASC, tile_y ASC, tile_x ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TileCoord{}
	for rows.Next() {
		var tile TileCoord
		if err := rows.Scan(&tile.X, &tile.Y); err != nil {
			return nil, err
		}
		out = append(out, tile)
	}
	return out, rows.Err()
}

func CorruptCheckpointForTest(path string, tile TileCoord, field string, value any) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	query := fmt.Sprintf(`UPDATE tile_checkpoints SET %s=? WHERE tile_x=? AND tile_y=?`, field)
	_, err = db.Exec(query, value, tile.X, tile.Y)
	return err
}
