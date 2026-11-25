package maskqc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type TileComponentRecord struct {
	TileID      string `json:"tileId"`
	CacheKey    string `json:"cacheKey"`
	ContentHash string `json:"contentHash"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Op          string `json:"op"`
	Alpha       []byte `json:"-"`
}

type CommitRecord struct {
	Seq              int64                 `json:"seq"`
	ProjectID        string                `json:"projectId"`
	StrokeID         string                `json:"strokeId"`
	OrderIndex       int                   `json:"orderIndex"`
	ResidualDigest   string                `json:"residualDigest"`
	OrderVersion     string                `json:"orderVersion"`
	ParamVersion     string                `json:"paramVersion"`
	AlgorithmVersion string                `json:"algorithmVersion"`
	CommittedAt      time.Time             `json:"committedAt"`
	Tiles            []TileComponentRecord `json:"tiles"`
}

type CheckpointStore struct {
	db *sql.DB
	mu sync.Mutex
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	dsn := path
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, &QCError{Code: CodeCheckpoint, Field: "sqlite", Message: err.Error()}
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &CheckpointStore{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS stroke_commits (
  project_id TEXT NOT NULL DEFAULT 'default',
  seq INTEGER NOT NULL,
  stroke_id TEXT NOT NULL,
  order_index INTEGER NOT NULL,
  residual_digest TEXT NOT NULL,
  order_version TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  committed_at TEXT NOT NULL,
  PRIMARY KEY(project_id, seq),
  UNIQUE(project_id, stroke_id)
);
CREATE TABLE IF NOT EXISTS tile_components (
  project_id TEXT NOT NULL DEFAULT 'default',
  commit_seq INTEGER NOT NULL,
  stroke_id TEXT NOT NULL,
  tile_id TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  op TEXT NOT NULL,
  alpha BLOB NOT NULL,
  PRIMARY KEY(project_id, commit_seq, tile_id),
  FOREIGN KEY(project_id, commit_seq) REFERENCES stroke_commits(project_id, seq)
);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`)
	if err != nil {
		return &QCError{Code: CodeCheckpoint, Field: "schema", Message: err.Error()}
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO meta(key,value) VALUES('algorithm_version', ?),('order_version', ?),('param_version', ?)`,
		AlgorithmVersion, OrderVersion, ParamVersion)
	return err
}

func (s *CheckpointStore) Close() error { return s.db.Close() }

func (s *CheckpointStore) ResetProject(projectID string) error {
	if projectID == "" {
		projectID = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return checkpointErr("reset begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM tile_components WHERE project_id=?`, projectID); err != nil {
		return checkpointErr("reset components", err)
	}
	if _, err := tx.Exec(`DELETE FROM stroke_commits WHERE project_id=?`, projectID); err != nil {
		return checkpointErr("reset commits", err)
	}
	return tx.Commit()
}

// CommitStroke serializes all writers and places the stroke row plus every
// affected tile component in one SQLite transaction.
func (s *CheckpointStore) CommitStroke(rec *CommitRecord) (int64, error) {
	if rec == nil || rec.StrokeID == "" {
		return 0, &QCError{Code: CodeCheckpoint, Field: "strokeId", Message: "stroke commit requires id"}
	}
	if rec.ProjectID == "" {
		rec.ProjectID = "default"
	}
	if rec.OrderVersion == "" {
		rec.OrderVersion = OrderVersion
	}
	if rec.ParamVersion == "" {
		rec.ParamVersion = ParamVersion
	}
	if rec.AlgorithmVersion == "" {
		rec.AlgorithmVersion = AlgorithmVersion
	}
	if rec.ResidualDigest == "" {
		rec.ResidualDigest = sha256Sum(nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, checkpointErr("begin", err)
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0)+1 FROM stroke_commits WHERE project_id=?`, rec.ProjectID).Scan(&nextSeq); err != nil {
		return 0, checkpointErr("sequence", err)
	}
	var existing string
	err = tx.QueryRow(`SELECT stroke_id FROM stroke_commits WHERE project_id=? AND stroke_id=?`, rec.ProjectID, rec.StrokeID).Scan(&existing)
	if err == nil {
		return 0, &QCError{Code: CodeCheckpoint, Field: "strokeId", Message: fmt.Sprintf("stroke %s already committed", rec.StrokeID)}
	}
	if err != sql.ErrNoRows {
		return 0, checkpointErr("idempotency", err)
	}
	now := time.Now().UTC()
	rec.Seq = nextSeq
	rec.CommittedAt = now
	if _, err := tx.Exec(`INSERT INTO stroke_commits
(project_id,seq,stroke_id,order_index,residual_digest,order_version,param_version,algorithm_version,committed_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
		rec.ProjectID, rec.Seq, rec.StrokeID, rec.OrderIndex, rec.ResidualDigest, rec.OrderVersion, rec.ParamVersion,
		rec.AlgorithmVersion, now.Format(time.RFC3339Nano)); err != nil {
		return 0, checkpointErr("insert stroke commit", err)
	}
	for _, tile := range rec.Tiles {
		if tile.CacheKey == "" || tile.ContentHash == "" {
			return 0, &QCError{Code: CodeCheckpoint, Field: "cacheKey", Message: "tile component requires cache key and hash"}
		}
		if _, err := tx.Exec(`INSERT INTO tile_components
(project_id,commit_seq,stroke_id,tile_id,cache_key,content_hash,width,height,op,alpha)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
			rec.ProjectID, rec.Seq, rec.StrokeID, tile.TileID, tile.CacheKey, tile.ContentHash,
			tile.Width, tile.Height, tile.Op, tile.Alpha); err != nil {
			return 0, checkpointErr("insert tile component", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, checkpointErr("commit", err)
	}
	return rec.Seq, nil
}

func checkpointErr(field string, err error) *QCError {
	return &QCError{Code: CodeCheckpoint, Field: field, Message: err.Error()}
}

func (s *CheckpointStore) LoadCommits(projectID string) ([]CommitRecord, error) {
	if projectID == "" {
		projectID = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT seq,stroke_id,order_index,residual_digest,order_version,param_version,algorithm_version,committed_at
FROM stroke_commits WHERE project_id=? ORDER BY seq ASC`, projectID)
	if err != nil {
		return nil, checkpointErr("load commits", err)
	}
	defer rows.Close()
	var commits []CommitRecord
	for rows.Next() {
		var c CommitRecord
		var at string
		if err := rows.Scan(&c.Seq, &c.StrokeID, &c.OrderIndex, &c.ResidualDigest, &c.OrderVersion,
			&c.ParamVersion, &c.AlgorithmVersion, &at); err != nil {
			return nil, checkpointErr("scan commit", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			c.CommittedAt = t
		}
		commits = append(commits, c)
	}
	for i := range commits {
		tiles, err := s.loadTiles(projectID, commits[i].Seq)
		if err != nil {
			return nil, err
		}
		commits[i].Tiles = tiles
	}
	return commits, nil
}

func (s *CheckpointStore) loadTiles(projectID string, seq int64) ([]TileComponentRecord, error) {
	rows, err := s.db.Query(`SELECT tile_id,cache_key,content_hash,width,height,op,alpha FROM tile_components WHERE project_id=? AND commit_seq=? ORDER BY tile_id`, projectID, seq)
	if err != nil {
		return nil, checkpointErr("load components", err)
	}
	defer rows.Close()
	var out []TileComponentRecord
	for rows.Next() {
		var r TileComponentRecord
		if err := rows.Scan(&r.TileID, &r.CacheKey, &r.ContentHash, &r.Width, &r.Height, &r.Op, &r.Alpha); err != nil {
			return nil, checkpointErr("scan component", err)
		}
		if HashBytes(r.Alpha) != r.ContentHash {
			return nil, &QCError{Code: CodeCheckpoint, Field: "contentHash", Message: "stored component hash mismatch", Details: map[string]any{"tileId": r.TileID}}
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *CheckpointStore) Sequence(projectID string) ([]string, error) {
	commits, err := s.LoadCommits(projectID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		out = append(out, c.StrokeID)
	}
	return out, nil
}

func MarshalComponentBlob(c *Component) ([]byte, error) {
	return json.Marshal(c)
}
