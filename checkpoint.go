package silhouette

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Checkpoint struct {
	SceneID          string       `json:"scene_id"`
	TileID           string       `json:"tile_id"`
	TileOrder        int          `json:"tile_order"`
	InputHash        string       `json:"input_hash"`
	VertexDigest     string       `json:"vertex_digest"`
	MaxChordHeight   int64        `json:"max_chord_height"`
	CoverageNum      int64        `json:"coverage_num"`
	CoverageDen      int64        `json:"coverage_den"`
	DependencySet    []string     `json:"dependency_set"`
	ContentHash      string       `json:"content_hash"`
	CacheKey         string       `json:"cache_key"`
	ToleranceVersion string       `json:"tolerance_version"`
	BudgetVersion    string       `json:"budget_version"`
	AlgorithmVersion string       `json:"algorithm_version"`
	GeometryVersion  string       `json:"geometry_version"`
	Tile             TileAssembly `json:"tile"`
	CommitSeq        int64        `json:"commit_seq"`
	CommittedAt      int64        `json:"committed_at"`
}

type CommitRecord struct {
	Seq          int64  `json:"seq"`
	SceneID      string `json:"scene_id"`
	TileID       string `json:"tile_id"`
	TileOrder    int    `json:"tile_order"`
	ContentHash  string `json:"content_hash"`
	CacheKey     string `json:"cache_key"`
	VertexDigest string `json:"vertex_digest"`
	CommittedAt  int64  `json:"committed_at"`
}

type CheckpointStore struct {
	db *sql.DB
	mu sync.Mutex
}

func OpenCheckpointStore(dsn string) (*CheckpointStore, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	store := &CheckpointStore{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS tile_checkpoints (
  scene_id TEXT NOT NULL,
  tile_id TEXT NOT NULL,
  tile_order INTEGER NOT NULL,
  input_hash TEXT NOT NULL,
  vertex_digest TEXT NOT NULL,
  max_chord_height INTEGER NOT NULL,
  coverage_num INTEGER NOT NULL,
  coverage_den INTEGER NOT NULL,
  dependency_set TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  tolerance_version TEXT NOT NULL,
  budget_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  geometry_version TEXT NOT NULL,
  tile_json TEXT NOT NULL,
  commit_seq INTEGER NOT NULL,
  committed_at INTEGER NOT NULL,
  PRIMARY KEY (scene_id, tile_id)
);
CREATE TABLE IF NOT EXISTS tile_commits (
  scene_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  tile_id TEXT NOT NULL,
  tile_order INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  vertex_digest TEXT NOT NULL,
  committed_at INTEGER NOT NULL,
  PRIMARY KEY (scene_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_tile_commits_scene ON tile_commits(scene_id, seq);
`
	_, err := s.db.Exec(schema)
	return err
}

func (s *CheckpointStore) Close() error { return s.db.Close() }

func tileDependencySet(tile TileAssembly) []string {
	set := []string{"parameter:budget", "parameter:tolerance"}
	for _, ts := range tile.Segments {
		set = append(set, "polyline:"+ts.SegmentID, "segment:"+ts.SegmentID)
		for j := 0; j < 4; j++ {
			set = append(set, fmt.Sprintf("control:%s:%d", ts.SegmentID, j))
		}
	}
	return uniqueSorted(set)
}

func tileVertexDigest(tile TileAssembly) string {
	type digestSegment struct {
		ID       string         `json:"id"`
		Metadata string         `json:"metadata"`
		Vertices []SampleVertex `json:"vertices"`
	}
	payload := make([]digestSegment, 0, len(tile.Segments))
	for _, ts := range tile.Segments {
		payload = append(payload, digestSegment{ID: ts.SegmentID, Metadata: ts.MetadataVersion, Vertices: ts.Vertices})
	}
	sort.SliceStable(payload, func(i, j int) bool { return payload[i].ID < payload[j].ID })
	return hashJSON(payload)
}

func tileMaxChord(tile TileAssembly) int64 {
	var max int64
	for _, ts := range tile.Segments {
		if ts.MaxChordHeight > max {
			max = ts.MaxChordHeight
		}
	}
	return max
}

// CommitTiles writes each tile in exactly one transaction. The store is
// single-writer and callers must provide tiles in stable order.
func (s *CheckpointStore) CommitTiles(ctx context.Context, sceneID string, tiles []TileAssembly, toleranceVersion, budgetVersion string) ([]CommitRecord, error) {
	records := make([]CommitRecord, 0, len(tiles))
	for _, tile := range tiles {
		record, err := s.CommitTile(ctx, sceneID, tile, toleranceVersion, budgetVersion)
		if err != nil {
			return records, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *CheckpointStore) CommitTile(ctx context.Context, sceneID string, tile TileAssembly, toleranceVersion, budgetVersion string) (CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deps := tileDependencySet(tile)
	depsJSON, err := json.Marshal(deps)
	if err != nil {
		return CommitRecord{}, err
	}
	tileJSON, err := json.Marshal(tile)
	if err != nil {
		return CommitRecord{}, err
	}
	digest := tileVertexDigest(tile)
	now := time.Now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CommitRecord{}, err
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM tile_commits WHERE scene_id=?`, sceneID).Scan(&seq); err != nil {
		return CommitRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO tile_checkpoints
 (scene_id,tile_id,tile_order,input_hash,vertex_digest,max_chord_height,coverage_num,coverage_den,
  dependency_set,content_hash,cache_key,tolerance_version,budget_version,algorithm_version,geometry_version,
  tile_json,commit_seq,committed_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?)
 ON CONFLICT(scene_id,tile_id) DO UPDATE SET
  tile_order=excluded.tile_order,input_hash=excluded.input_hash,vertex_digest=excluded.vertex_digest,
  max_chord_height=excluded.max_chord_height,coverage_num=excluded.coverage_num,coverage_den=excluded.coverage_den,
  dependency_set=excluded.dependency_set,content_hash=excluded.content_hash,cache_key=excluded.cache_key,
  tolerance_version=excluded.tolerance_version,budget_version=excluded.budget_version,
  algorithm_version=excluded.algorithm_version,geometry_version=excluded.geometry_version,
  tile_json=excluded.tile_json,committed_at=excluded.committed_at`,
		sceneID, tile.TileID, tile.Bounds.Order, tile.InputHash, digest, tileMaxChord(tile),
		tile.CoverageNum, tile.CoverageDen, string(depsJSON), tile.ContentHash, tile.CacheKey,
		toleranceVersion, budgetVersion, AlgorithmVersion, GeometryVersion, string(tileJSON), now)
	if err != nil {
		return CommitRecord{}, err
	}
	result, err := tx.ExecContext(ctx, `
 INSERT INTO tile_commits (scene_id,seq,tile_id,tile_order,content_hash,cache_key,vertex_digest,committed_at)
VALUES (?,?,?,?,?,?,?,?)`, sceneID, seq, tile.TileID, tile.Bounds.Order, tile.ContentHash, tile.CacheKey, digest, now)
	if err != nil {
		return CommitRecord{}, err
	}
	_ = result
	if _, err := tx.ExecContext(ctx, `UPDATE tile_checkpoints SET commit_seq=? WHERE scene_id=? AND tile_id=?`, seq, sceneID, tile.TileID); err != nil {
		return CommitRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return CommitRecord{}, err
	}
	return CommitRecord{
		Seq: seq, SceneID: sceneID, TileID: tile.TileID, TileOrder: tile.Bounds.Order,
		ContentHash: tile.ContentHash, CacheKey: tile.CacheKey, VertexDigest: digest, CommittedAt: now,
	}, nil
}

func (s *CheckpointStore) LoadCheckpoint(ctx context.Context, sceneID, tileID string) (*Checkpoint, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT scene_id,tile_id,tile_order,input_hash,vertex_digest,max_chord_height,coverage_num,coverage_den,
 dependency_set,content_hash,cache_key,tolerance_version,budget_version,algorithm_version,geometry_version,
 tile_json,commit_seq,committed_at
FROM tile_checkpoints WHERE scene_id=? AND tile_id=?`, sceneID, tileID)
	var c Checkpoint
	var depsJSON, tileJSON string
	err := row.Scan(&c.SceneID, &c.TileID, &c.TileOrder, &c.InputHash, &c.VertexDigest, &c.MaxChordHeight,
		&c.CoverageNum, &c.CoverageDen, &depsJSON, &c.ContentHash, &c.CacheKey, &c.ToleranceVersion,
		&c.BudgetVersion, &c.AlgorithmVersion, &c.GeometryVersion, &tileJSON, &c.CommitSeq, &c.CommittedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(depsJSON), &c.DependencySet); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(tileJSON), &c.Tile); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *CheckpointStore) ListCheckpoints(ctx context.Context, sceneID string) ([]Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT scene_id,tile_id,tile_order,input_hash,vertex_digest,max_chord_height,coverage_num,coverage_den,
 dependency_set,content_hash,cache_key,tolerance_version,budget_version,algorithm_version,geometry_version,
 tile_json,commit_seq,committed_at
FROM tile_checkpoints WHERE scene_id=? ORDER BY tile_order,tile_id`, sceneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		var c Checkpoint
		var depsJSON, tileJSON string
		if err := rows.Scan(&c.SceneID, &c.TileID, &c.TileOrder, &c.InputHash, &c.VertexDigest, &c.MaxChordHeight,
			&c.CoverageNum, &c.CoverageDen, &depsJSON, &c.ContentHash, &c.CacheKey, &c.ToleranceVersion,
			&c.BudgetVersion, &c.AlgorithmVersion, &c.GeometryVersion, &tileJSON, &c.CommitSeq, &c.CommittedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(depsJSON), &c.DependencySet); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tileJSON), &c.Tile); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *CheckpointStore) ListCommits(ctx context.Context, sceneID string) ([]CommitRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq,scene_id,tile_id,tile_order,content_hash,cache_key,vertex_digest,committed_at
FROM tile_commits WHERE scene_id=? ORDER BY seq`, sceneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitRecord
	for rows.Next() {
		var r CommitRecord
		if err := rows.Scan(&r.Seq, &r.SceneID, &r.TileID, &r.TileOrder, &r.ContentHash, &r.CacheKey, &r.VertexDigest, &r.CommittedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *CheckpointStore) ClearScene(ctx context.Context, sceneID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM tile_checkpoints WHERE scene_id=?`, sceneID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tile_commits WHERE scene_id=?`, sceneID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CheckpointStore) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

func checkpointMismatchReason(c *Checkpoint, tile TileAssembly, tolVersion, budgetVersion string) string {
	switch {
	case c.ContentHash != tile.ContentHash:
		return "content hash changed"
	case c.CacheKey != tile.CacheKey:
		return "cache key changed"
	case c.AlgorithmVersion != AlgorithmVersion:
		return "algorithm version changed"
	case c.GeometryVersion != GeometryVersion:
		return "geometry version changed"
	case c.ToleranceVersion != tolVersion:
		return "tolerance parameter version changed"
	case c.BudgetVersion != budgetVersion:
		return "budget parameter version changed"
	default:
		return ""
	}
}

func checkpointKey(sceneID, tileID string) string { return fmt.Sprintf("%s/%s", sceneID, tileID) }
