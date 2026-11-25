package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"maskqc"

	_ "modernc.org/sqlite"
)

// Store is the pure-Go SQLite persistence adapter.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens or initializes a local database file.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS snapshot_meta (
  project_id TEXT NOT NULL,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  PRIMARY KEY(project_id, key)
);
CREATE TABLE IF NOT EXISTS stroke_checkpoints (
  commit_seq INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  order_index INTEGER NOT NULL,
  stroke_seq INTEGER NOT NULL,
  root_hash TEXT NOT NULL,
  order_version TEXT NOT NULL,
  params_version TEXT NOT NULL,
  component_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS stroke_components (
  project_id TEXT NOT NULL,
  stroke_seq INTEGER NOT NULL,
  chunk_x INTEGER NOT NULL,
  chunk_y INTEGER NOT NULL,
  slot INTEGER NOT NULL,
  order_index INTEGER NOT NULL,
  commit_seq INTEGER NOT NULL,
  min_x INTEGER NOT NULL,
  min_y INTEGER NOT NULL,
  max_x INTEGER NOT NULL,
  max_y INTEGER NOT NULL,
  alpha BLOB NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  residual_digest TEXT NOT NULL,
  order_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  PRIMARY KEY(project_id, stroke_seq, chunk_x, chunk_y)
);
CREATE TABLE IF NOT EXISTS chunk_cache (
  cache_key TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  chunk_x INTEGER NOT NULL,
  chunk_y INTEGER NOT NULL,
  input_hash TEXT NOT NULL,
  output_hash TEXT NOT NULL,
  order_version TEXT NOT NULL,
  params_key TEXT NOT NULL,
  upstream_keys TEXT NOT NULL,
  alpha BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_components_project_order
  ON stroke_components(project_id, order_index, chunk_y, chunk_x);
CREATE INDEX IF NOT EXISTS idx_chunk_project_coord
  ON chunk_cache(project_id, chunk_x, chunk_y);
`)
	return err
}

// CommitStrokeCheckpoint replaces one stroke's current components in one tx.
func (s *Store) CommitStrokeCheckpoint(ctx context.Context, cp maskqc.StrokeCheckpoint) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	res, err := tx.ExecContext(ctx, `
INSERT INTO stroke_checkpoints
 (project_id, order_index, stroke_seq, root_hash, order_version, params_version, component_count)
 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		cp.ProjectID, cp.OrderIndex, cp.StrokeSeq, cp.RootContentHash, cp.OrderVersion, cp.ParamsVersion, len(cp.Components))
	if err != nil {
		return 0, err
	}
	commitSeq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM stroke_components WHERE project_id=? AND stroke_seq=?`,
		cp.ProjectID, cp.StrokeSeq); err != nil {
		return 0, err
	}

	for slot, rec := range cp.Components {
		b := rec.Bounds
		_, err := tx.ExecContext(ctx, `
INSERT INTO stroke_components
 (project_id, stroke_seq, chunk_x, chunk_y, slot, order_index, commit_seq,
  min_x, min_y, max_x, max_y, alpha, cache_key, content_hash,
  residual_digest, order_version, algorithm_version)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cp.ProjectID, rec.StrokeSeq, rec.Chunk.X, rec.Chunk.Y, slot,
			cp.OrderIndex, commitSeq, b.MinX, b.MinY, b.MaxX, b.MaxY,
			rec.Alpha, rec.CacheKey, rec.ContentHash, rec.ResidualDigest,
			rec.OrderVersion, rec.AlgorithmVersion)
		if err != nil {
			return 0, err
		}
	}

	meta := map[string]string{
		"root_content_hash": cp.RootContentHash,
		"order_version":     cp.OrderVersion,
		"params_version":    cp.ParamsVersion,
		"algorithm_version": maskqc.AlgorithmVersion,
		"schema_version":    cp.SchemaVersion,
		"last_commit_seq":   fmt.Sprintf("%d", commitSeq),
	}
	for key, value := range meta {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO snapshot_meta(project_id, key, value) VALUES(?, ?, ?)
ON CONFLICT(project_id, key) DO UPDATE SET value=excluded.value`,
			cp.ProjectID, key, value); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true
	return commitSeq, nil
}

// LoadComponentRecords loads only current stroke components in composition order.
func (s *Store) LoadComponentRecords(ctx context.Context, projectID string) ([]maskqc.ComponentRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT order_index, stroke_seq, chunk_x, chunk_y,
       min_x, min_y, max_x, max_y, alpha, cache_key, content_hash,
       residual_digest, order_version, algorithm_version, commit_seq
FROM stroke_components
WHERE project_id=?
ORDER BY order_index ASC, chunk_y ASC, chunk_x ASC, slot ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []maskqc.ComponentRecord
	for rows.Next() {
		var rec maskqc.ComponentRecord
		var x, y int
		if err := rows.Scan(
			&rec.OrderIndex, &rec.StrokeSeq, &x, &y,
			&rec.Bounds.MinX, &rec.Bounds.MinY, &rec.Bounds.MaxX, &rec.Bounds.MaxY,
			&rec.Alpha, &rec.CacheKey, &rec.ContentHash, &rec.ResidualDigest,
			&rec.OrderVersion, &rec.AlgorithmVersion, &rec.CommitSeq); err != nil {
			return nil, err
		}
		rec.ProjectID = projectID
		rec.Chunk = maskqc.ChunkCoord{X: x, Y: y}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// RecoverySnapshot validates at restart and atomically discards an invalid one.
func (s *Store) RecoverySnapshot(ctx context.Context, expected maskqc.SnapshotIdentity) (*maskqc.SnapshotValidation, []maskqc.ComponentRecord, error) {
	validation, err := s.ValidateSnapshot(ctx, expected.ProjectID, expected)
	if err != nil {
		return nil, nil, err
	}
	if !validation.Valid {
		if validation.Reason != "no_committed_snapshot" {
			if err := s.ResetProject(ctx, expected.ProjectID); err != nil {
				return validation, nil, err
			}
			validation.Invalidated = true
		}
		return validation, nil, nil
	}
	records, err := s.LoadComponentRecords(ctx, expected.ProjectID)
	if err != nil {
		return validation, nil, err
	}
	return validation, records, nil
}

// ValidateSnapshot compares restart identity and verifies component hashes.
func (s *Store) ValidateSnapshot(ctx context.Context, projectID string, expected maskqc.SnapshotIdentity) (*maskqc.SnapshotValidation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM snapshot_meta WHERE project_id=?`, projectID)
	if err != nil {
		return nil, err
	}
	meta := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			_ = rows.Close()
			return nil, err
		}
		meta[k] = v
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(meta) == 0 {
		return &maskqc.SnapshotValidation{Valid: false, Invalidated: false, Reason: "no_committed_snapshot", Expected: expected}, nil
	}
	stored := &maskqc.SnapshotIdentity{
		RootContentHash:  meta["root_content_hash"],
		OrderVersion:     meta["order_version"],
		ParamsVersion:    meta["params_version"],
		AlgorithmVersion: meta["algorithm_version"],
		SchemaVersion:    meta["schema_version"],
	}
	changed := []string{}
	reasons := []string{}
	if expected.RootContentHash != "" && stored.RootContentHash != expected.RootContentHash {
		changed = append(changed, "root_content_hash")
		reasons = append(reasons, "project content hash changed")
	}
	if expected.OrderVersion != "" && stored.OrderVersion != expected.OrderVersion {
		changed = append(changed, "order_version")
	}
	if expected.ParamsVersion != "" && stored.ParamsVersion != expected.ParamsVersion {
		changed = append(changed, "params_version")
	}
	if expected.AlgorithmVersion != "" && stored.AlgorithmVersion != expected.AlgorithmVersion {
		changed = append(changed, "algorithm_version")
		reasons = append(reasons, "algorithm version changed")
	}
	if expected.SchemaVersion != "" && stored.SchemaVersion != "" && stored.SchemaVersion != expected.SchemaVersion {
		changed = append(changed, "schema_version")
	}

	records, err := s.LoadComponentRecords(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		want := maskqc.HashBytes([]byte(fmt.Sprintf("component:%s:%d:%s", rec.AlgorithmVersion, rec.StrokeSeq, rec.Chunk.String())), rec.Alpha)
		if want != rec.ContentHash {
			changed = append(changed, fmt.Sprintf("content_hash:stroke-%d-chunk-%d-%d", rec.StrokeSeq, rec.Chunk.X, rec.Chunk.Y))
			reasons = append(reasons, "stored component content hash mismatch")
			break
		}
	}

	valid := len(changed) == 0
	reason := "ok"
	if !valid {
		reason = "snapshot identity/hash mismatch; discard old snapshot: " + fmt.Sprint(reasons)
	}
	return &maskqc.SnapshotValidation{Valid: valid, Invalidated: !valid, Reason: reason, ChangedFields: changed, Expected: expected, Stored: stored}, nil
}

// ResetProject atomically discards all checkpoint/cache state for a project.
func (s *Store) ResetProject(ctx context.Context, projectID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM stroke_components WHERE project_id=?`,
		`DELETE FROM stroke_checkpoints WHERE project_id=?`,
		`DELETE FROM snapshot_meta WHERE project_id=?`,
		`DELETE FROM chunk_cache WHERE project_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, projectID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// PutChunkCache persists one reusable tile cache entry.
func (s *Store) PutChunkCache(ctx context.Context, rec maskqc.ChunkCacheRecord) error {
	upstream, _ := json.Marshal(rec.UpstreamKeys)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO chunk_cache
 (cache_key, project_id, chunk_x, chunk_y, input_hash, output_hash,
  order_version, params_key, upstream_keys, alpha)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(cache_key) DO UPDATE SET
 project_id=excluded.project_id, chunk_x=excluded.chunk_x, chunk_y=excluded.chunk_y,
 input_hash=excluded.input_hash, output_hash=excluded.output_hash,
 order_version=excluded.order_version, params_key=excluded.params_key,
 upstream_keys=excluded.upstream_keys, alpha=excluded.alpha`,
		rec.CacheKey, rec.ProjectID, rec.Coord.X, rec.Coord.Y,
		rec.InputHash, rec.OutputHash, rec.OrderVersion, rec.ParamsKey,
		string(upstream), rec.Alpha)
	return err
}

// GetChunkCache reads a cache entry by its deterministic key.
func (s *Store) GetChunkCache(ctx context.Context, cacheKey string) (*maskqc.ChunkCacheRecord, error) {
	var rec maskqc.ChunkCacheRecord
	var upstream string
	err := s.db.QueryRowContext(ctx, `
SELECT project_id, chunk_x, chunk_y, input_hash, output_hash,
       order_version, params_key, upstream_keys, alpha
FROM chunk_cache WHERE cache_key=?`, cacheKey).Scan(
		&rec.ProjectID, &rec.Coord.X, &rec.Coord.Y, &rec.InputHash,
		&rec.OutputHash, &rec.OrderVersion, &rec.ParamsKey, &upstream, &rec.Alpha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(upstream), &rec.UpstreamKeys)
	rec.CacheKey = cacheKey
	return &rec, nil
}

// CheckpointHistoryEntry is one durable stroke transaction.
type CheckpointHistoryEntry struct {
	CommitSeq      int64  `json:"commit_seq"`
	OrderIndex     int    `json:"order_index"`
	StrokeSeq      int    `json:"stroke_seq"`
	RootHash       string `json:"root_hash"`
	OrderVersion   string `json:"order_version"`
	ParamsVersion  string `json:"params_version"`
	ComponentCount int    `json:"component_count"`
}

// CommitHistory returns stroke transactions in SQLite commit order.
func (s *Store) CommitHistory(ctx context.Context, projectID string) ([]CheckpointHistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT commit_seq, order_index, stroke_seq, root_hash, order_version, params_version, component_count
FROM stroke_checkpoints WHERE project_id=? ORDER BY commit_seq ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckpointHistoryEntry
	for rows.Next() {
		var e CheckpointHistoryEntry
		if err := rows.Scan(&e.CommitSeq, &e.OrderIndex, &e.StrokeSeq, &e.RootHash, &e.OrderVersion, &e.ParamsVersion, &e.ComponentCount); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CorruptComponentForTest flips one persisted byte to verify hash rejection.
func (s *Store) CorruptComponentForTest(ctx context.Context, projectID string, strokeSeq, chunkX, chunkY int) error {
	var alpha []byte
	err := s.db.QueryRowContext(ctx, `
SELECT alpha FROM stroke_components
WHERE project_id=? AND stroke_seq=? AND chunk_x=? AND chunk_y=?`,
		projectID, strokeSeq, chunkX, chunkY).Scan(&alpha)
	if err != nil {
		return err
	}
	if len(alpha) == 0 {
		return sql.ErrNoRows
	}
	alpha[0] ^= 0xFF
	_, err = s.db.ExecContext(ctx, `
UPDATE stroke_components SET alpha=?
WHERE project_id=? AND stroke_seq=? AND chunk_x=? AND chunk_y=?`,
		alpha, projectID, strokeSeq, chunkX, chunkY)
	return err
}
