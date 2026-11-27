package grid

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	_ "ballisticgrid/internal/sqlitedrv"
)

type CachedTile struct {
	TileID           TileID
	Seq              int
	ContentHash      string
	ColorDigest      string
	DepthDigest      string
	Color            []byte
	Depth            []uint32
	StructureVersion int
	CameraVersion    int
	AlgorithmVersion string
	Versions         SemanticVersions
	CoveragePermille int
	PolyPixels       int
	HitStats         map[string]int
}

type SnapshotStore struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

const createTileTableSQL = `CREATE TABLE IF NOT EXISTS tiles (
  tile_col INTEGER NOT NULL,
  tile_row INTEGER NOT NULL,
  seq INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  color_digest TEXT NOT NULL,
  depth_digest TEXT NOT NULL,
  color_blob BLOB NOT NULL,
  depth_blob BLOB NOT NULL,
  structure_version INTEGER NOT NULL,
  camera_version INTEGER NOT NULL,
  algorithm_version TEXT NOT NULL,
  versions_json TEXT NOT NULL,
  coverage_permille INTEGER NOT NULL,
  poly_pixels INTEGER NOT NULL,
  hit_stats_json TEXT NOT NULL,
  PRIMARY KEY (tile_col, tile_row)
)`

const upsertTileSQL = `INSERT OR REPLACE INTO tiles
(tile_col, tile_row, seq, content_hash, color_digest, depth_digest, color_blob, depth_blob,
 structure_version, camera_version, algorithm_version, versions_json, coverage_permille,
 poly_pixels, hit_stats_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const selectTileSQL = `SELECT tile_col, tile_row, seq, content_hash, color_digest, depth_digest,
 color_blob, depth_blob, structure_version, camera_version, algorithm_version, versions_json,
 coverage_permille, poly_pixels, hit_stats_json
FROM tiles WHERE tile_col = ? AND tile_row = ?`

const selectAllTilesSQL = `SELECT tile_col, tile_row, seq, content_hash, color_digest, depth_digest,
 color_blob, depth_blob, structure_version, camera_version, algorithm_version, versions_json,
 coverage_permille, poly_pixels, hit_stats_json
FROM tiles ORDER BY seq ASC, tile_row ASC, tile_col ASC`

func OpenSnapshot(path string) (*SnapshotStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(createTileTableSQL); err != nil {
		db.Close()
		return nil, err
	}
	return &SnapshotStore{db: db, path: path}, nil
}

func (s *SnapshotStore) Path() string { return s.path }

func (s *SnapshotStore) Close() error { return s.db.Close() }

// CommitTile writes every exact tile field in one serial transaction. The
// store mutex plus one connection guarantee commits cannot interleave.
func (s *SnapshotStore) CommitTile(t *Tile, cam Camera, versions SemanticVersions, hitStats map[string]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	versionsJSON, err := json.Marshal(versions)
	if err != nil {
		return err
	}
	statsJSON, err := json.Marshal(hitStats)
	if err != nil {
		return err
	}
	depthBlob := DepthBytes(t.Depth)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(upsertTileSQL,
		int64(t.ID.Col), int64(t.ID.Row), int64(t.Seq), t.ContentHash, t.ColorDigest, t.DepthDigest,
		append([]byte(nil), t.Color...), depthBlob, int64(versions.Structure),
		int64(cam.CameraVersion), AlgorithmVersion, string(versionsJSON),
		int64(t.CoveragePermille), int64(t.PolyCoverage), string(statsJSON),
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func scanTile(scanner interface {
	Scan(dest ...any) error
}) (*CachedTile, error) {
	var c CachedTile
	var col, row, seq, structure, camera, coverage, poly int64
	var contentHash, colorDigest, depthDigest, algorithm, versionsJSON, statsJSON string
	var colorBlob, depthBlob []byte
	if err := scanner.Scan(
		&col, &row, &seq, &contentHash, &colorDigest, &depthDigest, &colorBlob, &depthBlob,
		&structure, &camera, &algorithm, &versionsJSON, &coverage, &poly, &statsJSON,
	); err != nil {
		return nil, err
	}
	c.TileID = TileID{Col: int(col), Row: int(row)}
	c.Seq = int(seq)
	c.ContentHash = contentHash
	c.ColorDigest = colorDigest
	c.DepthDigest = depthDigest
	c.Color = append([]byte(nil), colorBlob...)
	c.Depth = ParseDepthBytes(depthBlob)
	c.StructureVersion = int(structure)
	c.CameraVersion = int(camera)
	c.AlgorithmVersion = algorithm
	c.CoveragePermille = int(coverage)
	c.PolyPixels = int(poly)
	c.HitStats = map[string]int{}
	if versionsJSON != "" {
		_ = json.Unmarshal([]byte(versionsJSON), &c.Versions)
	}
	if statsJSON != "" {
		_ = json.Unmarshal([]byte(statsJSON), &c.HitStats)
	}
	return &c, nil
}

func (s *SnapshotStore) LoadTile(col, row int) (*CachedTile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rowResult := s.db.QueryRow(selectTileSQL, int64(col), int64(row))
	c, err := scanTile(rowResult)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (s *SnapshotStore) LoadAll() ([]*CachedTile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(selectAllTilesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CachedTile
	for rows.Next() {
		c, err := scanTile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Recoverable reports whether a committed tile still matches the freshly
// projected content and the cache-key version components.
func (c *CachedTile) Recoverable(fresh *Tile, versions SemanticVersions, cameraVersion int) (bool, string) {
	if c == nil || fresh == nil {
		return false, "missing"
	}
	if c.ContentHash != fresh.ContentHash {
		return false, "contentHash"
	}
	if c.StructureVersion != versions.Structure {
		return false, "structureVersion"
	}
	if c.CameraVersion != cameraVersion {
		return false, "cameraVersion"
	}
	if c.AlgorithmVersion != AlgorithmVersion {
		return false, "algorithmVersion"
	}
	if c.ColorDigest != fresh.ColorDigest || c.DepthDigest != fresh.DepthDigest {
		return false, "bufferDigest"
	}
	return true, ""
}

func (c *CachedTile) PixelEqual(fresh *Tile) error {
	if len(c.Color) != len(fresh.Color) {
		return diffError(c.TileID, "colorBuffer.length", len(c.Color), len(fresh.Color))
	}
	for i := range fresh.Color {
		if c.Color[i] != fresh.Color[i] {
			return diffError(c.TileID, fmt.Sprintf("colorBuffer[%d]", i), c.Color[i], fresh.Color[i])
		}
	}
	if len(c.Depth) != len(fresh.Depth) {
		return diffError(c.TileID, "depthBuffer.length", len(c.Depth), len(fresh.Depth))
	}
	for i := range fresh.Depth {
		if c.Depth[i] != fresh.Depth[i] {
			return diffError(c.TileID, fmt.Sprintf("depthBuffer[%d]", i), c.Depth[i], fresh.Depth[i])
		}
	}
	return nil
}

func diffError(id TileID, field string, old, new any) *Error {
	return &Error{
		Code: CodeDiff, Tile: &id, Field: field, OldValue: fmt.Sprint(old),
		Message: fmt.Sprintf("pixel/state mismatch: old=%v new=%v", old, new),
	}
}
