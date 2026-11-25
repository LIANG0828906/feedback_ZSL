package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLite(path string) (*SQLiteStore, error) {
	if path == ":memory:" {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		s := &SQLiteStore{db: db}
		if err := s.migrate(context.Background()); err != nil {
			_ = db.Close()
			return nil, err
		}
		return s, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	slashPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	u := url.URL{Scheme: "file", Path: slashPath}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	u.RawQuery = q.Encode()
	dsn := u.String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &SQLiteStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS snapshot_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tile_checkpoints (
  commit_seq INTEGER PRIMARY KEY AUTOINCREMENT,
  tile_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  attempt INTEGER NOT NULL,
  param_version INTEGER NOT NULL,
  algorithm_version TEXT NOT NULL,
  vertex_digest TEXT NOT NULL,
  triangle_digest TEXT NOT NULL,
  boundary_stats_json TEXT NOT NULL,
  dependencies_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  content_json BLOB NOT NULL,
  vertex_count INTEGER NOT NULL,
  triangle_count INTEGER NOT NULL,
  committed_order INTEGER NOT NULL,
  committed_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tile_checkpoints_tile ON tile_checkpoints(tile_id, commit_seq DESC);
CREATE TABLE IF NOT EXISTS commit_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  tile_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  reason TEXT NOT NULL,
  committed_at INTEGER NOT NULL
);`)
	return err
}

func (s *SQLiteStore) Save(ctx context.Context, cp Checkpoint, reason string) (CommitRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CommitRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if cp.CommittedAt == 0 {
		cp.CommittedAt = time.Now().UnixNano()
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO tile_checkpoints (
 tile_id, generation, attempt, param_version, algorithm_version,
 vertex_digest, triangle_digest, boundary_stats_json, dependencies_json,
 content_hash, content_json, vertex_count, triangle_count, committed_order, committed_at
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		cp.TileID, cp.Generation, cp.Attempt, cp.ParamVersion, cp.AlgorithmVersion,
		cp.VertexDigest, cp.TriangleDigest, cp.BoundaryStatsJSON, cp.DependenciesJSON,
		cp.ContentHash, cp.ContentJSON, cp.VertexCount, cp.TriangleCount, cp.CommittedOrder, cp.CommittedAt,
	)
	if err != nil {
		return CommitRecord{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return CommitRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO commit_log(seq, tile_id, content_hash, reason, committed_at)
VALUES (?,?,?,?,?)`, seq, cp.TileID, cp.ContentHash, reason, cp.CommittedAt); err != nil {
		return CommitRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return CommitRecord{}, err
	}
	cp.CommitSeq = seq
	return CommitRecord{Seq: seq, TileID: cp.TileID, ContentHash: cp.ContentHash, Reason: reason, CommittedAt: cp.CommittedAt}, nil
}

func (s *SQLiteStore) Latest(ctx context.Context) (map[string]Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT commit_seq, tile_id, generation, attempt, param_version, algorithm_version,
       vertex_digest, triangle_digest, boundary_stats_json, dependencies_json,
       content_hash, content_json, vertex_count, triangle_count, committed_order, committed_at
FROM tile_checkpoints
ORDER BY commit_seq ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Checkpoint{}
	for rows.Next() {
		var cp Checkpoint
		if err := rows.Scan(&cp.CommitSeq, &cp.TileID, &cp.Generation, &cp.Attempt, &cp.ParamVersion,
			&cp.AlgorithmVersion, &cp.VertexDigest, &cp.TriangleDigest, &cp.BoundaryStatsJSON,
			&cp.DependenciesJSON, &cp.ContentHash, &cp.ContentJSON, &cp.VertexCount,
			&cp.TriangleCount, &cp.CommittedOrder, &cp.CommittedAt); err != nil {
			return nil, err
		}
		out[cp.TileID] = cp
	}
	return out, rows.Err()
}

func (s *SQLiteStore) History(ctx context.Context) ([]CommitRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, tile_id, content_hash, reason, committed_at FROM commit_log ORDER BY seq ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CommitRecord{}
	for rows.Next() {
		var r CommitRecord
		if err := rows.Scan(&r.Seq, &r.TileID, &r.ContentHash, &r.Reason, &r.CommittedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Reset(ctx context.Context, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM tile_checkpoints`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commit_log`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name IN ('tile_checkpoints','commit_log')`); err != nil {
		// sqlite_sequence may not exist until an AUTOINCREMENT row was written.
		_ = err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO snapshot_meta(key,value) VALUES('reset_reason',?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM snapshot_meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *SQLiteStore) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO snapshot_meta(key,value) VALUES(?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *SQLiteStore) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func SQLiteDSNInfo(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Sprintf("file:%s", filepath.ToSlash(path))
	}
	return "file:" + filepath.ToSlash(abs)
}
