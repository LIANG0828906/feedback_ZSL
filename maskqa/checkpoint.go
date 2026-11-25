package maskqa

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type CheckpointRecord struct {
	Seq            int64    `json:"seq"`
	ProjectID      string   `json:"project_id"`
	StrokeIndex    int      `json:"stroke_index"`
	TileID         int      `json:"tile_id"`
	Alpha          []uint16 `json:"-"`
	CacheKey       string   `json:"cache_key"`
	ContentHash    string   `json:"content_hash"`
	ResidualDigest string   `json:"residual_digest"`
	OrderVersion   string   `json:"order_version"`
	AlgoVersion    string   `json:"algo_version"`
	ParamVersion   string   `json:"param_version"`
	CommittedAt    int64    `json:"committed_at_unix_nano,omitempty"`
}

type TileOutputRecord struct {
	ProjectID    string   `json:"project_id"`
	TileID       int      `json:"tile_id"`
	AccumKey     string   `json:"accum_key"`
	InputHash    string   `json:"input_hash"`
	ContentHash  string   `json:"content_hash"`
	Mask         []byte   `json:"mask"`
	Alpha        []uint16 `json:"-"`
	OrderVersion string   `json:"order_version"`
	ParamVersion string   `json:"param_version"`
	AlgoVersion  string   `json:"algo_version"`
	UpdatedAt    int64    `json:"updated_at_unix_nano"`
}

type RecoveryReport struct {
	Fresh                   bool   `json:"fresh"`
	Invalidated             bool   `json:"invalidated"`
	Reason                  string `json:"reason,omitempty"`
	CommittedStrokeBoundary int    `json:"committed_stroke_boundary"`
	LoadedCommits           int    `json:"loaded_commits"`
}

type CheckpointStore struct {
	path string
	db   *sql.DB
	mu   sync.Mutex
}

// OpenCheckpoint opens/creates a pure-Go SQLite database. :memory: is also supported.
func OpenCheckpoint(path string) (*CheckpointStore, error) {
	if path != ":memory:" && path != "" {
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &CheckpointStore{path: path, db: db}
	if err := store.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) Close() error { return s.db.Close() }

func (s *CheckpointStore) initSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_meta (
  project_id TEXT PRIMARY KEY,
  residual_digest TEXT NOT NULL,
  order_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  param_version TEXT NOT NULL,
  updated_nano INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS commit_sequence (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  stroke_index INTEGER NOT NULL,
  tile_id INTEGER NOT NULL,
  component_blob BLOB NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  residual_digest TEXT NOT NULL,
  order_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  param_version TEXT NOT NULL,
  committed_nano INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS stroke_checkpoints (
  project_id TEXT NOT NULL,
  stroke_index INTEGER NOT NULL,
  tile_id INTEGER NOT NULL,
  commit_seq INTEGER NOT NULL,
  component_blob BLOB NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  residual_digest TEXT NOT NULL,
  order_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  param_version TEXT NOT NULL,
  PRIMARY KEY(project_id, stroke_index, tile_id)
);
CREATE TABLE IF NOT EXISTS tile_outputs (
  project_id TEXT NOT NULL,
  tile_id INTEGER NOT NULL,
  accum_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  mask_blob BLOB NOT NULL,
  alpha_blob BLOB NOT NULL,
  order_version TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  updated_nano INTEGER NOT NULL,
  PRIMARY KEY(project_id, tile_id)
);
`)
	return err
}

func EncodeAlpha(alpha []uint16) []byte {
	b := make([]byte, len(alpha)*2)
	for i, v := range alpha {
		binary.BigEndian.PutUint16(b[i*2:], v)
	}
	return b
}

func DecodeAlpha(b []byte) []uint16 {
	alpha := make([]uint16, len(b)/2)
	for i := range alpha {
		alpha[i] = binary.BigEndian.Uint16(b[i*2:])
	}
	return alpha
}

// EnsureMeta inserts expected versions for a fresh project snapshot.
func (s *CheckpointStore) EnsureMeta(ctx context.Context, projectID, residualDigest, orderVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO schema_meta
	(project_id,residual_digest,order_version,algo_version,param_version,updated_nano)
	VALUES (?,?,?,?,?,?)`, projectID, residualDigest, orderVersion, AlgorithmVersion, ParameterVersion, time.Now().UnixNano())
	return err
}

// CommitStroke writes all affected tile components for one stroke in one serial transaction.
func (s *CheckpointStore) CommitStroke(ctx context.Context, recs []CheckpointRecord) ([]CheckpointRecord, error) {
	if len(recs) == 0 {
		return recs, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	committed := make([]CheckpointRecord, 0, len(recs))
	for _, rec := range recs {
		blob := EncodeAlpha(rec.Alpha)
		if rec.ContentHash == "" {
			rec.ContentHash = HashBytes(blob)
		}
		if rec.CommittedAt == 0 {
			rec.CommittedAt = time.Now().UnixNano()
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO commit_sequence
		(project_id,stroke_index,tile_id,component_blob,cache_key,content_hash,residual_digest,order_version,algo_version,param_version,committed_nano)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			rec.ProjectID, rec.StrokeIndex, rec.TileID, blob, rec.CacheKey, rec.ContentHash,
			rec.ResidualDigest, rec.OrderVersion, rec.AlgoVersion, rec.ParamVersion, rec.CommittedAt)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		seq, err := res.LastInsertId()
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		rec.Seq = seq
		_, err = tx.ExecContext(ctx, `INSERT INTO stroke_checkpoints
		(project_id,stroke_index,tile_id,commit_seq,component_blob,cache_key,content_hash,residual_digest,order_version,algo_version,param_version)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id,stroke_index,tile_id) DO UPDATE SET
		  commit_seq=excluded.commit_seq,
		  component_blob=excluded.component_blob,
		  cache_key=excluded.cache_key,
		  content_hash=excluded.content_hash,
		  residual_digest=excluded.residual_digest,
		  order_version=excluded.order_version,
		  algo_version=excluded.algo_version,
		  param_version=excluded.param_version`,
			rec.ProjectID, rec.StrokeIndex, rec.TileID, seq, blob, rec.CacheKey, rec.ContentHash,
			rec.ResidualDigest, rec.OrderVersion, rec.AlgoVersion, rec.ParamVersion)
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		committed = append(committed, rec)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return committed, nil
}

func (s *CheckpointStore) SaveTileOutput(ctx context.Context, out TileOutputRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tile_outputs
	(project_id,tile_id,accum_key,input_hash,content_hash,mask_blob,alpha_blob,order_version,param_version,algo_version,updated_nano)
	VALUES (?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(project_id,tile_id) DO UPDATE SET
	  accum_key=excluded.accum_key, input_hash=excluded.input_hash, content_hash=excluded.content_hash,
	  mask_blob=excluded.mask_blob, alpha_blob=excluded.alpha_blob, updated_nano=excluded.updated_nano`,
		out.ProjectID, out.TileID, out.AccumKey, out.InputHash, out.ContentHash, out.Mask, EncodeAlpha(out.Alpha),
		out.OrderVersion, out.ParamVersion, out.AlgoVersion, time.Now().UnixNano())
	return err
}

func (s *CheckpointStore) LoadTileOutput(ctx context.Context, projectID string, tileID int) (*TileOutputRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT accum_key,input_hash,content_hash,mask_blob,alpha_blob,order_version,param_version,algo_version
	FROM tile_outputs WHERE project_id=? AND tile_id=?`, projectID, tileID)
	var out TileOutputRecord
	var alphaBlob []byte
	out.ProjectID = projectID
	out.TileID = tileID
	err := row.Scan(&out.AccumKey, &out.InputHash, &out.ContentHash, &out.Mask, &alphaBlob, &out.OrderVersion, &out.ParamVersion, &out.AlgoVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.Alpha = DecodeAlpha(alphaBlob)
	if HashBytes(out.Mask) != out.ContentHash {
		return nil, &Error{Code: CodeDIFF, Field: "tile_outputs.content_hash", FirstTile: tileID, Message: "persisted tile output hash mismatch"}
	}
	return &out, nil
}

func checkpointKey(project string, stroke, tile int) string {
	return fmt.Sprintf("%s|%d|%d", project, stroke, tile)
}

// LoadCheckpoints loads current stroke checkpoint components.
func (s *CheckpointStore) LoadCheckpoints(ctx context.Context, projectID string) (map[string]CheckpointRecord, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_meta WHERE project_id=?`, projectID).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return map[string]CheckpointRecord{}, nil
	}
	rs, err := s.db.QueryContext(ctx, `SELECT stroke_index,tile_id,component_blob,cache_key,content_hash,residual_digest,order_version,algo_version,param_version,commit_seq
	FROM stroke_checkpoints WHERE project_id=? ORDER BY stroke_index,tile_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]CheckpointRecord{}
	for rs.Next() {
		var rec CheckpointRecord
		var blob []byte
		if err := rs.Scan(&rec.StrokeIndex, &rec.TileID, &blob, &rec.CacheKey, &rec.ContentHash, &rec.ResidualDigest, &rec.OrderVersion, &rec.AlgoVersion, &rec.ParamVersion, &rec.Seq); err != nil {
			return nil, err
		}
		rec.ProjectID = projectID
		rec.Alpha = DecodeAlpha(blob)
		out[checkpointKey(projectID, rec.StrokeIndex, rec.TileID)] = rec
	}
	return out, rs.Err()
}

func (s *CheckpointStore) LoadCommitSequence(ctx context.Context, projectID string) ([]CheckpointRecord, error) {
	rs, err := s.db.QueryContext(ctx, `SELECT seq,stroke_index,tile_id,cache_key,content_hash,residual_digest,order_version,algo_version,param_version,committed_nano
	FROM commit_sequence WHERE project_id=? ORDER BY seq`, projectID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []CheckpointRecord
	for rs.Next() {
		var rec CheckpointRecord
		rec.ProjectID = projectID
		if err := rs.Scan(&rec.Seq, &rec.StrokeIndex, &rec.TileID, &rec.CacheKey, &rec.ContentHash, &rec.ResidualDigest, &rec.OrderVersion, &rec.AlgoVersion, &rec.ParamVersion, &rec.CommittedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rs.Err()
}

// Recover validates versions/hashes and returns the contiguous committed stroke boundary.
func (s *CheckpointStore) Recover(ctx context.Context, projectID, residualDigest, orderVersion string) (*RecoveryReport, map[string]CheckpointRecord, error) {
	rep := &RecoveryReport{}
	var gotResidual, gotOrder, gotAlgo, gotParam string
	err := s.db.QueryRowContext(ctx, `SELECT residual_digest,order_version,algo_version,param_version FROM schema_meta WHERE project_id=?`, projectID).
		Scan(&gotResidual, &gotOrder, &gotAlgo, &gotParam)
	if errors.Is(err, sql.ErrNoRows) {
		rep.Fresh = true
		return rep, map[string]CheckpointRecord{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if gotAlgo != AlgorithmVersion || gotParam != ParameterVersion || gotOrder != orderVersion || gotResidual != residualDigest {
		rep.Invalidated = true
		rep.Reason = fmt.Sprintf("version/digest mismatch: algo=%s/%s order=%s/%s residual=%s/%s", gotAlgo, AlgorithmVersion, gotOrder, orderVersion, gotResidual, residualDigest)
		if err := s.ResetProject(ctx, projectID); err != nil {
			return nil, nil, err
		}
		return rep, map[string]CheckpointRecord{}, nil
	}

	checkpoints, err := s.LoadCheckpoints(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	for _, rec := range checkpoints {
		if HashBytes(EncodeAlpha(rec.Alpha)) != rec.ContentHash {
			rep.Invalidated = true
			rep.Reason = fmt.Sprintf("content hash mismatch at stroke=%d tile=%d", rec.StrokeIndex, rec.TileID)
			if err := s.ResetProject(ctx, projectID); err != nil {
				return nil, nil, err
			}
			return rep, map[string]CheckpointRecord{}, nil
		}
		if rec.AlgoVersion != AlgorithmVersion || rec.OrderVersion != orderVersion {
			rep.Invalidated = true
			rep.Reason = "checkpoint version mismatch"
			if err := s.ResetProject(ctx, projectID); err != nil {
				return nil, nil, err
			}
			return rep, map[string]CheckpointRecord{}, nil
		}
	}
	rep.LoadedCommits = len(checkpoints)
	present := map[int]bool{}
	for _, rec := range checkpoints {
		present[rec.StrokeIndex] = true
	}
	boundary := 0
	for i := 1; present[i]; i++ {
		boundary = i
	}
	rep.CommittedStrokeBoundary = boundary
	return rep, checkpoints, nil
}

func (s *CheckpointStore) ResetProject(ctx context.Context, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range []string{
		`DELETE FROM commit_sequence WHERE project_id=?`,
		`DELETE FROM stroke_checkpoints WHERE project_id=?`,
		`DELETE FROM tile_outputs WHERE project_id=?`,
		`DELETE FROM schema_meta WHERE project_id=?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, projectID); err != nil {
			return err
		}
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name='commit_sequence'`)
	return nil
}

// CorruptCheckpointForTest deliberately flips a component byte to test recovery invalidation.
func (s *CheckpointStore) CorruptCheckpointForTest(ctx context.Context, projectID string, stroke, tile int) error {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT component_blob FROM stroke_checkpoints WHERE project_id=? AND stroke_index=? AND tile_id=?`, projectID, stroke, tile).Scan(&blob)
	if err != nil {
		return err
	}
	if len(blob) == 0 {
		return fmt.Errorf("empty blob")
	}
	blob[0] ^= 0xFF
	_, err = s.db.ExecContext(ctx, `UPDATE stroke_checkpoints SET component_blob=? WHERE project_id=? AND stroke_index=? AND tile_id=?`, blob, projectID, stroke, tile)
	return err
}

func (s *CheckpointStore) CorruptMetaVersionForTest(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schema_meta SET algo_version='broken-test-version' WHERE project_id=?`, projectID)
	return err
}
