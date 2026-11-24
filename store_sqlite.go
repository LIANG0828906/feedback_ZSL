package terrain

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
	path    string
}

type CommitBlockRequest struct {
	SnapshotID       string
	ProjectID        string
	BlockNo          int
	Mesh             *Mesh
	Dependencies     []Dependency
	BoundaryStats    []*BoundaryReport
	VertexDigest     string
	TriangleDigest   string
	ContentHash      string
	ParameterVersion string
	AlgorithmVersion string
	CommittedAt      time.Time
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = "terrain-checkpoint.db"
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	st := &Store{db: db, path: path}
	if err := st.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS snapshots (
  snapshot_id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  parameter_version TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS blocks (
  snapshot_id TEXT NOT NULL,
  block_no INTEGER NOT NULL,
  vertex_count INTEGER NOT NULL,
  triangle_count INTEGER NOT NULL,
  vertex_digest TEXT NOT NULL,
  triangle_digest TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  mesh_json TEXT NOT NULL,
  stats_json TEXT NOT NULL,
  dependencies_json TEXT NOT NULL,
  boundary_stats_json TEXT NOT NULL,
  parameter_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  committed_at TEXT NOT NULL,
  PRIMARY KEY(snapshot_id, block_no)
);
CREATE TABLE IF NOT EXISTS dependencies (
  snapshot_id TEXT NOT NULL,
  block_no INTEGER NOT NULL,
  depends_on INTEGER NOT NULL,
  boundary_id TEXT NOT NULL,
  direction TEXT NOT NULL,
  PRIMARY KEY(snapshot_id, block_no, boundary_id, depends_on)
);
CREATE TABLE IF NOT EXISTS commit_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  snapshot_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  block_no INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  committed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS commit_sequence (
  snapshot_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  block_no INTEGER NOT NULL,
  content_hash TEXT NOT NULL DEFAULT '',
  committed_at TEXT NOT NULL,
  PRIMARY KEY(snapshot_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_commit_snapshot ON commit_events(snapshot_id, seq);
CREATE TABLE IF NOT EXISTS boundary_stats (
  snapshot_id TEXT NOT NULL,
  boundary_id TEXT NOT NULL,
  block_a INTEGER NOT NULL,
  block_b INTEGER NOT NULL,
  max_fold_angle REAL NOT NULL,
  category TEXT NOT NULL,
  report_json TEXT NOT NULL,
  PRIMARY KEY(snapshot_id, boundary_id)
);`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SnapshotVersions(snapshotID string) (algo, param string, exists bool, err error) {
	row := s.db.QueryRow(`SELECT algorithm_version, parameter_version FROM snapshots WHERE snapshot_id=?`, snapshotID)
	if scanErr := row.Scan(&algo, &param); scanErr != nil {
		if scanErr == sql.ErrNoRows {
			return "", "", false, nil
		}
		return "", "", false, scanErr
	}
	return algo, param, true, nil
}

func (s *Store) ResetSnapshot(snapshotID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM blocks WHERE snapshot_id=?`,
		`DELETE FROM dependencies WHERE snapshot_id=?`,
		`DELETE FROM commit_events WHERE snapshot_id=?`,
		`DELETE FROM commit_sequence WHERE snapshot_id=?`,
		`DELETE FROM boundary_stats WHERE snapshot_id=?`,
		`DELETE FROM snapshots WHERE snapshot_id=?`,
	} {
		if _, err := tx.Exec(q, snapshotID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) EnsureSnapshot(snapshotID, projectID, algo, param string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`INSERT INTO snapshots(snapshot_id,project_id,algorithm_version,parameter_version,status,created_at,updated_at)
VALUES(?,?,?,?, 'building', ?, ?)
ON CONFLICT(snapshot_id) DO UPDATE SET project_id=excluded.project_id, algorithm_version=excluded.algorithm_version,
 parameter_version=excluded.parameter_version, updated_at=excluded.updated_at`,
		snapshotID, projectID, algo, param, now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) CommitBlock(req CommitBlockRequest) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if req.CommittedAt.IsZero() {
		req.CommittedAt = time.Now()
	}
	meshJSON, err := json.Marshal(req.Mesh)
	if err != nil {
		return err
	}
	statsJSON, err := json.Marshal(req.Mesh.TriangleErrors)
	if err != nil {
		return err
	}
	depsJSON, err := json.Marshal(req.Dependencies)
	if err != nil {
		return err
	}
	boundaryJSON, err := json.Marshal(req.BoundaryStats)
	if err != nil {
		return err
	}
	now := req.CommittedAt.UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO blocks(snapshot_id,block_no,vertex_count,triangle_count,vertex_digest,triangle_digest,
content_hash,mesh_json,stats_json,dependencies_json,boundary_stats_json,parameter_version,algorithm_version,committed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(snapshot_id,block_no) DO UPDATE SET vertex_count=excluded.vertex_count,triangle_count=excluded.triangle_count,
vertex_digest=excluded.vertex_digest,triangle_digest=excluded.triangle_digest,content_hash=excluded.content_hash,
mesh_json=excluded.mesh_json,stats_json=excluded.stats_json,dependencies_json=excluded.dependencies_json,
boundary_stats_json=excluded.boundary_stats_json,parameter_version=excluded.parameter_version,
algorithm_version=excluded.algorithm_version,committed_at=excluded.committed_at`,
		req.SnapshotID, req.BlockNo, len(req.Mesh.Vertices), len(req.Mesh.Triangles), req.VertexDigest, req.TriangleDigest,
		req.ContentHash, string(meshJSON), string(statsJSON), string(depsJSON), string(boundaryJSON),
		req.ParameterVersion, req.AlgorithmVersion, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM dependencies WHERE snapshot_id=? AND block_no=?`, req.SnapshotID, req.BlockNo); err != nil {
		return err
	}
	for _, dep := range req.Dependencies {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO dependencies(snapshot_id,block_no,depends_on,boundary_id,direction) VALUES(?,?,?,?,?)`,
			req.SnapshotID, req.BlockNo, dep.DependsOn, dep.BoundaryID, dep.Direction); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO commit_events(snapshot_id,seq,block_no,content_hash,committed_at) VALUES(?,?,?,?,?)`,
		req.SnapshotID, req.BlockNo, req.BlockNo, req.ContentHash, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO commit_sequence(snapshot_id,seq,block_no,content_hash,committed_at) VALUES(?,?,?,?,?)
ON CONFLICT(snapshot_id,seq) DO UPDATE SET block_no=excluded.block_no,content_hash=excluded.content_hash,committed_at=excluded.committed_at`,
		req.SnapshotID, req.BlockNo, req.BlockNo, req.ContentHash, now); err != nil {
		return err
	}
	for _, br := range req.BoundaryStats {
		reportJSON, err := json.Marshal(br)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO boundary_stats(snapshot_id,boundary_id,block_a,block_b,max_fold_angle,category,report_json)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(snapshot_id,boundary_id) DO UPDATE SET max_fold_angle=excluded.max_fold_angle,
category=excluded.category,report_json=excluded.report_json`,
			req.SnapshotID, br.BoundaryID, br.BlockA, br.BlockB, br.MaxFoldAngleDeg, br.Category, string(reportJSON)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type LoadedBlock struct {
	BlockNo        int
	Mesh           *Mesh
	ContentHash    string
	VertexDigest   string
	TriangleDigest string
	CommittedAt    string
}

func (s *Store) LoadBlocks(snapshotID, algoVer, paramVer string) (map[int]LoadedBlock, []CommitRecord, error) {
	rows, err := s.db.Query(`SELECT block_no, mesh_json, content_hash, vertex_digest, triangle_digest, committed_at,
algorithm_version, parameter_version FROM blocks WHERE snapshot_id=? ORDER BY block_no`, snapshotID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[int]LoadedBlock{}
	for rows.Next() {
		var no int
		var meshJSON, hash, vd, td, committedAt, algo, param string
		if err := rows.Scan(&no, &meshJSON, &hash, &vd, &td, &committedAt, &algo, &param); err != nil {
			return nil, nil, err
		}
		if algo != algoVer || param != paramVer {
			return nil, nil, &CodedError{Code: CodeSchema, Path: "checkpoint.versions", BlockNo: &no, Message: fmt.Sprintf("stored versions %s/%s do not match %s/%s", algo, param, algoVer, paramVer)}
		}
		var mesh Mesh
		if err := json.Unmarshal([]byte(meshJSON), &mesh); err != nil {
			return nil, nil, &CodedError{Code: CodeSchema, Path: "checkpoint.blocks.mesh_json", BlockNo: &no, Message: err.Error()}
		}
		out[no] = LoadedBlock{BlockNo: no, Mesh: &mesh, ContentHash: hash, VertexDigest: vd, TriangleDigest: td, CommittedAt: committedAt}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	eventRows, err := s.db.Query(`SELECT seq, block_no, content_hash, committed_at FROM commit_sequence WHERE snapshot_id=? ORDER BY seq`, snapshotID)
	if err != nil {
		return nil, nil, err
	}
	defer eventRows.Close()
	var seq []CommitRecord
	for eventRows.Next() {
		var rec CommitRecord
		if err := eventRows.Scan(&rec.Seq, &rec.BlockNo, &rec.Hash, &rec.CommittedAt); err != nil {
			return nil, nil, err
		}
		seq = append(seq, rec)
	}
	return out, seq, eventRows.Err()
}

func (s *Store) CompleteSnapshot(snapshotID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.Exec(`UPDATE snapshots SET status='complete', updated_at=? WHERE snapshot_id=?`, time.Now().UTC().Format(time.RFC3339Nano), snapshotID)
	return err
}
