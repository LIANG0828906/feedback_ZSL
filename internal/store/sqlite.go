package store

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"cadlab/internal/model"
	"cadlab/internal/render"
)

type SnapshotMeta struct {
	Version          int64  `json:"version"`
	ContentHash      string `json:"contentHash"`
	AlgorithmVersion string `json:"algorithmVersion"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	TileCols         int    `json:"tileCols"`
	TileRows         int    `json:"tileRows"`
	Path             string `json:"path"`
}

type ResumeResult struct {
	Meta  SnapshotMeta        `json:"meta"`
	Tiles []render.TileResult `json:"tiles"`
}

type Store struct {
	mu         sync.Mutex
	db         *sql.DB
	path       string
	commits    []int
	writeCount int
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("empty checkpoint path")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS snapshots (
  version INTEGER PRIMARY KEY,
  content_hash TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  tile_cols INTEGER NOT NULL,
  tile_rows INTEGER NOT NULL,
  complete INTEGER NOT NULL,
  path TEXT NOT NULL,
  scene_json BLOB NOT NULL,
  created_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tile_checkpoints (
  version INTEGER NOT NULL,
  tile_id INTEGER NOT NULL,
  ordinal INTEGER NOT NULL,
  x INTEGER NOT NULL,
  y INTEGER NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  color_gz BLOB NOT NULL,
  depth_gz BLOB NOT NULL,
  stats_json TEXT NOT NULL,
  duration_ns INTEGER NOT NULL,
  PRIMARY KEY (version, tile_id),
  FOREIGN KEY (version) REFERENCES snapshots(version) ON DELETE CASCADE
);`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) Path() string { return s.path }

func (s *Store) Begin(meta SnapshotMeta, sceneJSON []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM tile_checkpoints`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM snapshots`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO snapshots
		(version, content_hash, algorithm_version, width, height, tile_cols, tile_rows, complete, path, scene_json, created_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		meta.Version, meta.ContentHash, meta.AlgorithmVersion, meta.Width, meta.Height,
		meta.TileCols, meta.TileRows, meta.Path, sceneJSON, time.Now().UnixNano()); err != nil {
		return err
	}
	s.commits = nil
	s.writeCount = 0
	return tx.Commit()
}

func (s *Store) LatestScene() ([]byte, SnapshotMeta, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sceneJSON []byte
	var meta SnapshotMeta
	var complete bool
	err := s.db.QueryRow(`SELECT version, content_hash, algorithm_version, width, height,
		tile_cols, tile_rows, complete, path, scene_json FROM snapshots LIMIT 1`).Scan(
		&meta.Version, &meta.ContentHash, &meta.AlgorithmVersion, &meta.Width, &meta.Height,
		&meta.TileCols, &meta.TileRows, &complete, &meta.Path, &sceneJSON,
	)
	if err == sql.ErrNoRows {
		return nil, SnapshotMeta{}, false, nil
	}
	if err != nil {
		return nil, SnapshotMeta{}, false, err
	}
	return sceneJSON, meta, complete, nil
}

func (s *Store) CommitTile(meta SnapshotMeta, tile render.TileResult, ordinal int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	color, err := gzipBytes(tile.Color)
	if err != nil {
		return err
	}
	depthRaw := make([]byte, len(tile.Depth)*4)
	for i, d := range tile.Depth {
		binary.LittleEndian.PutUint32(depthRaw[i*4:], mathFloat32bits(d))
	}
	depth, err := gzipBytes(depthRaw)
	if err != nil {
		return err
	}
	stats, err := json.Marshal(tile.Stats)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR REPLACE INTO tile_checkpoints
		(version, tile_id, ordinal, x, y, width, height, content_hash, algorithm_version,
		 color_gz, depth_gz, stats_json, duration_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		meta.Version, tile.ID, ordinal, tile.X, tile.Y, tile.W, tile.H,
		meta.ContentHash, meta.AlgorithmVersion, color, depth, string(stats), int64(tile.Duration)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.commits = append(s.commits, tile.ID)
	s.writeCount++
	return nil
}

func (s *Store) Finalize(meta SnapshotMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE snapshots SET complete=1 WHERE version=? AND content_hash=? AND algorithm_version=?`,
		meta.Version, meta.ContentHash, meta.AlgorithmVersion)
	return err
}

func (s *Store) WriteOrder() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.commits...)
}

func (s *Store) WriteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeCount
}

func (s *Store) Load(meta SnapshotMeta, requiredTiles []int) (*ResumeResult, *model.Diagnostic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var stored SnapshotMeta
	var complete bool
	err := s.db.QueryRow(`SELECT version, content_hash, algorithm_version, width, height, tile_cols, tile_rows, complete, path
		FROM snapshots LIMIT 1`).Scan(
		&stored.Version, &stored.ContentHash, &stored.AlgorithmVersion,
		&stored.Width, &stored.Height, &stored.TileCols, &stored.TileRows, &complete, &stored.Path,
	)
	if err == sql.ErrNoRows {
		return nil, &model.Diagnostic{
			Code: model.CodeRecover, Message: "no persisted checkpoint", PatchID: -1, TileID: -1,
			Field: "snapshot",
		}
	}
	if err != nil {
		return nil, model.Diag(model.CodeRecover, err.Error())
	}
	if stored.Version != meta.Version || stored.ContentHash != meta.ContentHash ||
		stored.AlgorithmVersion != meta.AlgorithmVersion || stored.Width != meta.Width ||
		stored.Height != meta.Height || stored.TileCols != meta.TileCols || stored.TileRows != meta.TileRows {
		conflicts := s.tileIDsLocked(stored.Version)
		s.invalidateLocked()
		return nil, &model.Diagnostic{
			Code: model.CodeSnap, Message: "snapshot metadata does not match current scene",
			PatchID: -1, TileID: -1, Field: "snapshotContentHash",
			VersionA: stored.Version, VersionB: meta.Version,
			ConflictingTiles: conflicts,
		}
	}
	if !complete {
		return nil, &model.Diagnostic{
			Code: model.CodeRecover, Message: "checkpoint is incomplete after interruption",
			PatchID: -1, TileID: -1, Field: "snapshot.complete",
			ConflictingTiles: s.tileIDsLocked(stored.Version),
		}
	}
	rows, err := s.db.Query(`SELECT tile_id, ordinal, x, y, width, height, content_hash, algorithm_version,
		color_gz, depth_gz, stats_json, duration_ns FROM tile_checkpoints WHERE version=? ORDER BY ordinal, tile_id`, stored.Version)
	if err != nil {
		return nil, model.Diag(model.CodeRecover, err.Error())
	}
	defer rows.Close()
	var tiles []render.TileResult
	var conflicts []int
	seen := map[int]bool{}
	for rows.Next() {
		var id, ordinal, x, y, w, h int
		var hash, algorithm string
		var colorGZ, depthGZ []byte
		var statsJSON string
		var duration int64
		if err := rows.Scan(&id, &ordinal, &x, &y, &w, &h, &hash, &algorithm, &colorGZ, &depthGZ, &statsJSON, &duration); err != nil {
			return nil, model.Diag(model.CodeRecover, err.Error())
		}
		seen[id] = true
		if hash != meta.ContentHash || algorithm != meta.AlgorithmVersion {
			conflicts = append(conflicts, id)
			continue
		}
		color, err := ungzipBytes(colorGZ)
		if err != nil {
			return nil, model.DiagTile(model.CodeRecover, -1, id, "colorBuffer", err.Error())
		}
		depthBytes, err := ungzipBytes(depthGZ)
		if err != nil {
			return nil, model.DiagTile(model.CodeRecover, -1, id, "depthBuffer", err.Error())
		}
		if len(depthBytes)%4 != 0 {
			return nil, model.DiagTile(model.CodeRecover, -1, id, "depthBuffer", "invalid depth byte length")
		}
		depth := make([]float32, len(depthBytes)/4)
		for i := range depth {
			depth[i] = mathFloat32frombits(binary.LittleEndian.Uint32(depthBytes[i*4:]))
		}
		var stats render.TileStats
		if err := json.Unmarshal([]byte(statsJSON), &stats); err != nil {
			return nil, model.DiagTile(model.CodeRecover, -1, id, "stats", err.Error())
		}
		tiles = append(tiles, render.TileResult{
			ID: id, X: x, Y: y, W: w, H: h, Color: color, Depth: depth,
			Stats: stats, Duration: time.Duration(duration), Version: meta.Version, Path: meta.Path,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, model.Diag(model.CodeRecover, err.Error())
	}
	if len(conflicts) > 0 {
		s.invalidateLocked()
		sort.Ints(conflicts)
		return nil, &model.Diagnostic{
			Code: model.CodeSnap, Message: "mixed snapshot tile metadata detected; entire snapshot invalidated",
			PatchID: -1, TileID: -1, Field: "tile.contentHash",
			ConflictingTiles: conflicts,
		}
	}
	var missing []int
	for _, id := range requiredTiles {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	if len(tiles) != len(requiredTiles) || len(missing) > 0 {
		sort.Ints(missing)
		return nil, &model.Diagnostic{
			Code: model.CodeRecover, Message: "persisted checkpoint is missing committed tiles",
			PatchID: -1, TileID: -1, Field: "tileCheckpoints",
			ConflictingTiles: missing,
		}
	}
	sort.Slice(tiles, func(i, j int) bool { return tiles[i].ID < tiles[j].ID })
	return &ResumeResult{Meta: stored, Tiles: tiles}, nil
}

func (s *Store) InvalidateAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateLocked()
}

func (s *Store) invalidateLocked() {
	s.db.Exec(`DELETE FROM tile_checkpoints`)
	s.db.Exec(`DELETE FROM snapshots`)
	s.commits = nil
	s.writeCount = 0
}

func (s *Store) tileIDsLocked(version int64) []int {
	rows, err := s.db.Query(`SELECT tile_id FROM tile_checkpoints WHERE version=? ORDER BY tile_id`, version)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func gzipBytes(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(src); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ungzipBytes(src []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
