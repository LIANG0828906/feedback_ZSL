package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"ballistic/internal/kernel"

	_ "modernc.org/sqlite"
)

type Key struct {
	FrameID               string
	TileID                string
	ContentHash           string
	SceneStructureVersion string
	SemanticVersionKey    string
	CameraVersion         string
	AlgorithmVersion      string
}

func (k Key) CacheKey() string {
	return fmt.Sprintf("tile=%s|content=%s|scene=%s|camera=%s|algo=%s",
		k.TileID, k.ContentHash, k.SceneStructureVersion, k.CameraVersion, k.AlgorithmVersion)
}

type CommitInput struct {
	Key
	CommitOrder        int
	ColorDigest        string
	DepthDigest        string
	CoverageBp         int
	ObstacleCoverageBp int
	PolylineCoverageBp int
	PolylineShareBp    int
	BounceCount        int
	FirstHitCount      int
	Color              []byte
	Depth              []uint32
}

type Record struct {
	Key
	CommitOrder        int    `json:"commit_order"`
	ColorDigest        string `json:"color_digest"`
	DepthDigest        string `json:"depth_digest"`
	CoverageBp         int    `json:"coverage_bp"`
	ObstacleCoverageBp int    `json:"obstacle_coverage_bp"`
	PolylineCoverageBp int    `json:"polyline_coverage_bp"`
	PolylineShareBp    int    `json:"polyline_share_bp"`
	BounceCount        int    `json:"bounce_count"`
	FirstHitCount      int    `json:"first_hit_count"`
	StaleReason        string `json:"stale_reason,omitempty"`
}

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS tile_current (
			frame_id TEXT NOT NULL,
			tile_id TEXT NOT NULL,
			commit_order INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			scene_structure_version TEXT NOT NULL,
			semantic_version_key TEXT NOT NULL,
			camera_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			color_digest TEXT NOT NULL,
			depth_digest TEXT NOT NULL,
			coverage_bp INTEGER NOT NULL,
			obstacle_coverage_bp INTEGER NOT NULL,
			polyline_coverage_bp INTEGER NOT NULL,
			polyline_share_bp INTEGER NOT NULL,
			bounce_count INTEGER NOT NULL,
			first_hit_count INTEGER NOT NULL,
			color_blob BLOB NOT NULL,
			depth_blob BLOB NOT NULL,
			PRIMARY KEY(frame_id, tile_id)
		)`,
		`CREATE TABLE IF NOT EXISTS tile_commit_log (
			log_id INTEGER PRIMARY KEY AUTOINCREMENT,
			frame_id TEXT NOT NULL,
			tile_id TEXT NOT NULL,
			commit_order INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			scene_structure_version TEXT NOT NULL,
			camera_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			color_digest TEXT NOT NULL,
			depth_digest TEXT NOT NULL,
			committed_in_order INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_commit_log_frame_order
		 ON tile_commit_log(frame_id, log_id)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CommitTile(ctx context.Context, in CommitInput) error {
	depthBlob := make([]byte, len(in.Depth)*4)
	for i, value := range in.Depth {
		depthBlob[i*4] = byte(value)
		depthBlob[i*4+1] = byte(value >> 8)
		depthBlob[i*4+2] = byte(value >> 16)
		depthBlob[i*4+3] = byte(value >> 24)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tile_commit_log(
			frame_id, tile_id, commit_order, content_hash,
			scene_structure_version, camera_version, algorithm_version,
			color_digest, depth_digest, committed_in_order)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.FrameID, in.TileID, in.CommitOrder, in.ContentHash,
		in.SceneStructureVersion, in.CameraVersion, in.AlgorithmVersion,
		in.ColorDigest, in.DepthDigest, in.CommitOrder); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tile_current(
			frame_id, tile_id, commit_order, content_hash,
			scene_structure_version, semantic_version_key, camera_version, algorithm_version,
			color_digest, depth_digest, coverage_bp, obstacle_coverage_bp,
			polyline_coverage_bp, polyline_share_bp, bounce_count, first_hit_count,
			color_blob, depth_blob)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(frame_id, tile_id) DO UPDATE SET
			commit_order=excluded.commit_order,
			content_hash=excluded.content_hash,
			scene_structure_version=excluded.scene_structure_version,
			semantic_version_key=excluded.semantic_version_key,
			camera_version=excluded.camera_version,
			algorithm_version=excluded.algorithm_version,
			color_digest=excluded.color_digest,
			depth_digest=excluded.depth_digest,
			coverage_bp=excluded.coverage_bp,
			obstacle_coverage_bp=excluded.obstacle_coverage_bp,
			polyline_coverage_bp=excluded.polyline_coverage_bp,
			polyline_share_bp=excluded.polyline_share_bp,
			bounce_count=excluded.bounce_count,
			first_hit_count=excluded.first_hit_count,
			color_blob=excluded.color_blob,
			depth_blob=excluded.depth_blob`,
		in.FrameID, in.TileID, in.CommitOrder, in.ContentHash,
		in.SceneStructureVersion, in.SemanticVersionKey, in.CameraVersion, in.AlgorithmVersion,
		in.ColorDigest, in.DepthDigest, in.CoverageBp, in.ObstacleCoverageBp,
		in.PolylineCoverageBp, in.PolylineShareBp, in.BounceCount, in.FirstHitCount,
		in.Color, depthBlob); err != nil {
		return err
	}
	return tx.Commit()
}

var errStale = errors.New("stale snapshot")

func (s *Store) LoadValidTile(ctx context.Context, key Key, camera kernel.Camera) (*kernel.TileResult, error) {
	rec, colorBlob, depthBlob, err := s.loadRaw(ctx, key.FrameID, key.TileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	staleReason := ""
	switch {
	case rec.ContentHash != key.ContentHash:
		staleReason = "content_hash_mismatch"
	case rec.SceneStructureVersion != key.SceneStructureVersion:
		staleReason = "scene_structure_version_mismatch"
	case rec.CameraVersion != key.CameraVersion:
		staleReason = "camera_version_mismatch"
	case rec.AlgorithmVersion != key.AlgorithmVersion:
		staleReason = "algorithm_version_mismatch"
	}
	if staleReason != "" {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM tile_current WHERE frame_id=? AND tile_id=?`,
			key.FrameID, key.TileID); err != nil {
			return nil, err
		}
		return nil, nil
	}

	pixelN := camera.TileW * camera.TileH
	if len(colorBlob) != pixelN*4 || len(depthBlob) != pixelN*4 {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM tile_current WHERE frame_id=? AND tile_id=?`,
			key.FrameID, key.TileID); err != nil {
			return nil, err
		}
		return nil, nil
	}
	depth := make([]uint32, pixelN)
	for i := range depth {
		depth[i] = uint32(depthBlob[i*4]) |
			uint32(depthBlob[i*4+1])<<8 |
			uint32(depthBlob[i*4+2])<<16 |
			uint32(depthBlob[i*4+3])<<24
	}
	return &kernel.TileResult{
		TileID:              key.TileID,
		ContentHash:         rec.ContentHash,
		AlgorithmVersion:    rec.AlgorithmVersion,
		ColorDigest:         rec.ColorDigest,
		DepthDigest:         rec.DepthDigest,
		CoverageBp:          rec.CoverageBp,
		ObstacleCoverageBp:  rec.ObstacleCoverageBp,
		PolylineCoverageBp:  rec.PolylineCoverageBp,
		PolylineShareBp:     rec.PolylineShareBp,
		BounceCount:         rec.BounceCount,
		FirstHitMarkerCount: rec.FirstHitCount,
		Color:               colorBlob,
		Depth:               depth,
	}, nil
}

func (s *Store) loadRaw(ctx context.Context, frameID, tileID string) (Record, []byte, []byte, error) {
	var rec Record
	var colorBlob, depthBlob []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT frame_id, tile_id, commit_order, content_hash, scene_structure_version,
		       semantic_version_key, camera_version, algorithm_version, color_digest, depth_digest,
		       coverage_bp, obstacle_coverage_bp, polyline_coverage_bp, polyline_share_bp,
		       bounce_count, first_hit_count, color_blob, depth_blob
		  FROM tile_current WHERE frame_id=? AND tile_id=?`, frameID, tileID).Scan(
		&rec.FrameID, &rec.TileID, &rec.CommitOrder, &rec.ContentHash,
		&rec.SceneStructureVersion, &rec.SemanticVersionKey, &rec.CameraVersion, &rec.AlgorithmVersion,
		&rec.ColorDigest, &rec.DepthDigest, &rec.CoverageBp, &rec.ObstacleCoverageBp,
		&rec.PolylineCoverageBp, &rec.PolylineShareBp, &rec.BounceCount, &rec.FirstHitCount,
		&colorBlob, &depthBlob)
	if err != nil {
		return Record{}, nil, nil, err
	}
	return rec, colorBlob, depthBlob, nil
}

func (s *Store) ListCurrent(ctx context.Context, frameID string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT frame_id, tile_id, commit_order, content_hash, scene_structure_version,
		       semantic_version_key, camera_version, algorithm_version, color_digest, depth_digest,
		       coverage_bp, obstacle_coverage_bp, polyline_coverage_bp, polyline_share_bp,
		       bounce_count, first_hit_count
		  FROM tile_current WHERE frame_id=? ORDER BY commit_order`, frameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []Record{}
	for rows.Next() {
		var rec Record
		if err := rows.Scan(
			&rec.FrameID, &rec.TileID, &rec.CommitOrder, &rec.ContentHash,
			&rec.SceneStructureVersion, &rec.SemanticVersionKey, &rec.CameraVersion, &rec.AlgorithmVersion,
			&rec.ColorDigest, &rec.DepthDigest, &rec.CoverageBp, &rec.ObstacleCoverageBp,
			&rec.PolylineCoverageBp, &rec.PolylineShareBp, &rec.BounceCount, &rec.FirstHitCount); err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (s *Store) CommitLog(ctx context.Context, frameID string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT frame_id, tile_id, commit_order, content_hash, scene_structure_version,
		       '' AS semantic_version_key, camera_version, algorithm_version, color_digest, depth_digest,
		       0,0,0,0,0,0
		  FROM tile_commit_log WHERE frame_id=? ORDER BY log_id`, frameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []Record{}
	for rows.Next() {
		var rec Record
		if err := rows.Scan(
			&rec.FrameID, &rec.TileID, &rec.CommitOrder, &rec.ContentHash,
			&rec.SceneStructureVersion, &rec.SemanticVersionKey, &rec.CameraVersion, &rec.AlgorithmVersion,
			&rec.ColorDigest, &rec.DepthDigest,
			&rec.CoverageBp, &rec.ObstacleCoverageBp, &rec.PolylineCoverageBp, &rec.PolylineShareBp,
			&rec.BounceCount, &rec.FirstHitCount); err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (s *Store) CorruptContentHash(ctx context.Context, frameID, tileID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE tile_current SET content_hash='corrupt' WHERE frame_id=? AND tile_id=?`, frameID, tileID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("snapshot %s/%s not found", frameID, tileID)
	}
	return nil
}
