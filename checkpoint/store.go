package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"maskqc/model"
)

const schemaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS project_meta (
  project_id TEXT PRIMARY KEY,
  content_hash TEXT NOT NULL,
  conversion_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS stroke_checkpoint (
  project_id TEXT NOT NULL,
  stroke_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  status TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  component_digest TEXT NOT NULL,
  residual_digest TEXT NOT NULL,
  order_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  conversion_version TEXT NOT NULL,
  commit_sequence INTEGER NOT NULL,
  error_message TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (project_id, stroke_id)
);
CREATE TABLE IF NOT EXISTS stroke_component (
  project_id TEXT NOT NULL,
  stroke_id TEXT NOT NULL,
  block_index INTEGER NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  component_digest TEXT NOT NULL,
  coverage BLOB NOT NULL,
  residual_digest TEXT NOT NULL,
  order_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  conversion_version TEXT NOT NULL,
  PRIMARY KEY (project_id, stroke_id, block_index)
);
CREATE TABLE IF NOT EXISTS block_checkpoint (
  project_id TEXT NOT NULL,
  block_index INTEGER NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  composite_digest TEXT NOT NULL,
  stroke_ids TEXT NOT NULL,
  mask BLOB NOT NULL,
  order_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  conversion_version TEXT NOT NULL,
  commit_sequence INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (project_id, block_index)
);
CREATE TABLE IF NOT EXISTS invalidation_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  object_id TEXT NOT NULL,
  reason TEXT NOT NULL,
  created_at TEXT NOT NULL
);
`

type Stamp struct {
	ProjectID         string
	ContentHash       string
	ConversionVersion string
	AlgorithmVersion  string
	Width             int
	Height            int
}

type ComponentCommit struct {
	BlockIndex      int
	BlockX          int
	BlockY          int
	ContentHash     string
	CacheKey        string
	ComponentDigest string
	Coverage        []byte
	ResidualDigest  string
	OrderVersion    string
}

type StrokeCommit struct {
	Stamp           Stamp
	StrokeID        string
	Seq             int
	Status          string
	InputHash       string
	CacheKey        string
	ComponentDigest string
	ResidualDigest  string
	OrderVersion    string
	CommitSequence  int
	ErrorMessage    string
	Components      []ComponentCommit
}

type BlockCommit struct {
	Stamp           Stamp
	BlockIndex      int
	BlockX          int
	BlockY          int
	ContentHash     string
	CacheKey        string
	CompositeDigest string
	StrokeIDs       []string
	Mask            []byte
	OrderVersion    string
	CommitSequence  int
}

type ComponentRecord struct {
	BlockIndex      int
	BlockX          int
	BlockY          int
	ContentHash     string
	CacheKey        string
	ComponentDigest string
	Coverage        []byte
	ResidualDigest  string
	OrderVersion    string
}

type StrokeRecord struct {
	StrokeID          string
	Seq               int
	Status            string
	InputHash         string
	CacheKey          string
	ComponentDigest   string
	ResidualDigest    string
	OrderVersion      string
	AlgorithmVersion  string
	ConversionVersion string
	CommitSequence    int
	ErrorMessage      string
	Components        []ComponentRecord
}

type BlockRecord struct {
	BlockIndex        int
	BlockX            int
	BlockY            int
	ContentHash       string
	CacheKey          string
	CompositeDigest   string
	StrokeIDs         []string
	Mask              []byte
	OrderVersion      string
	AlgorithmVersion  string
	ConversionVersion string
	CommitSequence    int
}

type RestoreResult struct {
	Valid       bool
	Invalidated bool
	Reason      string
	Strokes     map[string]StrokeRecord
	Blocks      map[int]BlockRecord
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, &model.Error{Code: model.CodeParam, Path: "dbPath", Field: "dbPath", Message: "database path is empty"}
	}
	dsn := "file:" + path
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) CommitStroke(commit StrokeCommit) error {
	if commit.Status != "committed" && commit.Status != "failed" {
		return &model.Error{
			Code: model.CodeParam, Path: "stroke.status", ObjectID: commit.StrokeID,
			Field: "status", Message: "checkpoint status must be committed or failed", Allowed: "committed,failed",
		}
	}
	if commit.Stamp.ProjectID == "" {
		return model.NewError(model.CodeParam, "stamp.projectId", "", "project id is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO project_meta(project_id, content_hash, conversion_version, algorithm_version, width, height, updated_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(project_id) DO UPDATE SET
		  content_hash=excluded.content_hash,
		  conversion_version=excluded.conversion_version,
		  algorithm_version=excluded.algorithm_version,
		  width=excluded.width,
		  height=excluded.height,
		  updated_at=excluded.updated_at`,
		commit.Stamp.ProjectID, commit.Stamp.ContentHash, commit.Stamp.ConversionVersion,
		commit.Stamp.AlgorithmVersion, commit.Stamp.Width, commit.Stamp.Height, now,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO stroke_checkpoint(
		  project_id, stroke_id, seq, status, input_hash, cache_key, component_digest,
		  residual_digest, order_version, algorithm_version, conversion_version,
		  commit_sequence, error_message, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id, stroke_id) DO UPDATE SET
		  seq=excluded.seq,
		  status=excluded.status,
		  input_hash=excluded.input_hash,
		  cache_key=excluded.cache_key,
		  component_digest=excluded.component_digest,
		  residual_digest=excluded.residual_digest,
		  order_version=excluded.order_version,
		  algorithm_version=excluded.algorithm_version,
		  conversion_version=excluded.conversion_version,
		  commit_sequence=excluded.commit_sequence,
		  error_message=excluded.error_message,
		  updated_at=excluded.updated_at`,
		commit.Stamp.ProjectID, commit.StrokeID, commit.Seq, commit.Status, commit.InputHash,
		commit.CacheKey, commit.ComponentDigest, commit.ResidualDigest, commit.OrderVersion,
		commit.Stamp.AlgorithmVersion, commit.Stamp.ConversionVersion, commit.CommitSequence,
		commit.ErrorMessage, now,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM stroke_component WHERE project_id=? AND stroke_id=?`, commit.Stamp.ProjectID, commit.StrokeID); err != nil {
		return err
	}
	for _, component := range commit.Components {
		digest := component.ComponentDigest
		if digest == "" {
			digest = model.HashBytes(component.Coverage)
		}
		if digest != model.HashBytes(component.Coverage) {
			return &model.Error{
				Code: model.CodeSchema, Path: "component.digest", ObjectID: commit.StrokeID,
				Field: "componentDigest", Message: "coverage component digest does not match bytes",
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stroke_component(
			  project_id, stroke_id, block_index, block_x, block_y, content_hash, cache_key,
			  component_digest, coverage, residual_digest, order_version, algorithm_version, conversion_version
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			commit.Stamp.ProjectID, commit.StrokeID, component.BlockIndex, component.BlockX, component.BlockY,
			component.ContentHash, component.CacheKey, digest, component.Coverage, component.ResidualDigest,
			component.OrderVersion, commit.Stamp.AlgorithmVersion, commit.Stamp.ConversionVersion,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CommitBlock(commit BlockCommit) error {
	if commit.Stamp.ProjectID == "" {
		return model.NewError(model.CodeParam, "stamp.projectId", "", "project id is empty")
	}
	if commit.CompositeDigest == "" {
		commit.CompositeDigest = model.HashBytes(commit.Mask)
	}
	if commit.CompositeDigest != model.HashBytes(commit.Mask) {
		return model.NewError(model.CodeSchema, "block.compositeDigest", "", "composite digest does not match bytes")
	}
	strokeIDs, err := json.Marshal(commit.StrokeIDs)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO block_checkpoint(
		  project_id, block_index, block_x, block_y, content_hash, cache_key, composite_digest,
		  stroke_ids, mask, order_version, algorithm_version, conversion_version, commit_sequence, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id, block_index) DO UPDATE SET
		  block_x=excluded.block_x,
		  block_y=excluded.block_y,
		  content_hash=excluded.content_hash,
		  cache_key=excluded.cache_key,
		  composite_digest=excluded.composite_digest,
		  stroke_ids=excluded.stroke_ids,
		  mask=excluded.mask,
		  order_version=excluded.order_version,
		  algorithm_version=excluded.algorithm_version,
		  conversion_version=excluded.conversion_version,
		  commit_sequence=excluded.commit_sequence,
		  updated_at=excluded.updated_at`,
		commit.Stamp.ProjectID, commit.BlockIndex, commit.BlockX, commit.BlockY,
		commit.ContentHash, commit.CacheKey, commit.CompositeDigest, string(strokeIDs), commit.Mask,
		commit.OrderVersion, commit.Stamp.AlgorithmVersion, commit.Stamp.ConversionVersion,
		commit.CommitSequence, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkStrokeFailure(stamp Stamp, strokeID string, seq int, inputHash, reason string, sequence int) error {
	return s.CommitStroke(StrokeCommit{
		Stamp: stamp, StrokeID: strokeID, Seq: seq, Status: "failed",
		InputHash: inputHash, CacheKey: "", ComponentDigest: "", ResidualDigest: "",
		OrderVersion: "", CommitSequence: sequence, ErrorMessage: reason,
	})
}

func (s *Store) Restore(stamp Stamp) (*RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	var stored Stamp
	var updatedAt string
	row := s.db.QueryRowContext(ctx, `
		SELECT content_hash, conversion_version, algorithm_version, width, height, updated_at
		FROM project_meta WHERE project_id=?`, stamp.ProjectID)
	if err := row.Scan(&stored.ContentHash, &stored.ConversionVersion, &stored.AlgorithmVersion, &stored.Width, &stored.Height, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return &RestoreResult{Valid: false, Reason: "no committed checkpoint"}, nil
		}
		return nil, err
	}
	reason := ""
	switch {
	case stored.ContentHash != stamp.ContentHash:
		reason = "project content hash mismatch"
	case stored.ConversionVersion != stamp.ConversionVersion:
		reason = "conversion version mismatch"
	case stored.AlgorithmVersion != stamp.AlgorithmVersion:
		reason = "algorithm version mismatch"
	case stored.Width != stamp.Width || stored.Height != stamp.Height:
		reason = "canvas dimensions mismatch"
	}
	if reason != "" {
		if err := s.invalidateProjectLocked(ctx, stamp.ProjectID, reason); err != nil {
			return nil, err
		}
		return &RestoreResult{Valid: false, Invalidated: true, Reason: reason}, nil
	}
	result := &RestoreResult{Valid: true, Strokes: map[string]StrokeRecord{}, Blocks: map[int]BlockRecord{}}
	strokeRows, err := s.db.QueryContext(ctx, `
		SELECT stroke_id, seq, status, input_hash, cache_key, component_digest, residual_digest,
		       order_version, algorithm_version, conversion_version, commit_sequence, error_message
		FROM stroke_checkpoint WHERE project_id=? ORDER BY seq`, stamp.ProjectID)
	if err != nil {
		return nil, err
	}
	for strokeRows.Next() {
		var record StrokeRecord
		if err := strokeRows.Scan(&record.StrokeID, &record.Seq, &record.Status, &record.InputHash,
			&record.CacheKey, &record.ComponentDigest, &record.ResidualDigest, &record.OrderVersion,
			&record.AlgorithmVersion, &record.ConversionVersion, &record.CommitSequence, &record.ErrorMessage); err != nil {
			strokeRows.Close()
			return nil, err
		}
		if record.AlgorithmVersion != stamp.AlgorithmVersion || record.ConversionVersion != stamp.ConversionVersion {
			strokeRows.Close()
			_ = s.invalidateProjectLocked(ctx, stamp.ProjectID, "stroke version mismatch")
			return &RestoreResult{Valid: false, Invalidated: true, Reason: "stroke version mismatch"}, nil
		}
		result.Strokes[record.StrokeID] = record
	}
	if err := strokeRows.Close(); err != nil {
		return nil, err
	}
	componentRows, err := s.db.QueryContext(ctx, `
		SELECT stroke_id, block_index, block_x, block_y, content_hash, cache_key, component_digest,
		       coverage, residual_digest, order_version
		FROM stroke_component WHERE project_id=? ORDER BY stroke_id, block_index`, stamp.ProjectID)
	if err != nil {
		return nil, err
	}
	for componentRows.Next() {
		var strokeID string
		var component ComponentRecord
		if err := componentRows.Scan(&strokeID, &component.BlockIndex, &component.BlockX, &component.BlockY,
			&component.ContentHash, &component.CacheKey, &component.ComponentDigest, &component.Coverage,
			&component.ResidualDigest, &component.OrderVersion); err != nil {
			componentRows.Close()
			return nil, err
		}
		if component.ComponentDigest != model.HashBytes(component.Coverage) {
			componentRows.Close()
			_ = s.invalidateProjectLocked(ctx, stamp.ProjectID, "stroke component content mismatch")
			return &RestoreResult{Valid: false, Invalidated: true, Reason: "stroke component content mismatch"}, nil
		}
		stroke := result.Strokes[strokeID]
		stroke.Components = append(stroke.Components, component)
		result.Strokes[strokeID] = stroke
	}
	if err := componentRows.Close(); err != nil {
		return nil, err
	}
	blockRows, err := s.db.QueryContext(ctx, `
		SELECT block_index, block_x, block_y, content_hash, cache_key, composite_digest, stroke_ids,
		       mask, order_version, algorithm_version, conversion_version, commit_sequence
		FROM block_checkpoint WHERE project_id=? ORDER BY block_index`, stamp.ProjectID)
	if err != nil {
		return nil, err
	}
	for blockRows.Next() {
		var record BlockRecord
		var strokeIDs string
		if err := blockRows.Scan(&record.BlockIndex, &record.BlockX, &record.BlockY, &record.ContentHash,
			&record.CacheKey, &record.CompositeDigest, &strokeIDs, &record.Mask, &record.OrderVersion,
			&record.AlgorithmVersion, &record.ConversionVersion, &record.CommitSequence); err != nil {
			blockRows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(strokeIDs), &record.StrokeIDs); err != nil {
			blockRows.Close()
			return nil, err
		}
		if record.CompositeDigest != model.HashBytes(record.Mask) {
			blockRows.Close()
			_ = s.invalidateProjectLocked(ctx, stamp.ProjectID, "block content mismatch")
			return &RestoreResult{Valid: false, Invalidated: true, Reason: "block content mismatch"}, nil
		}
		if record.AlgorithmVersion != stamp.AlgorithmVersion || record.ConversionVersion != stamp.ConversionVersion {
			blockRows.Close()
			_ = s.invalidateProjectLocked(ctx, stamp.ProjectID, "block version mismatch")
			return &RestoreResult{Valid: false, Invalidated: true, Reason: "block version mismatch"}, nil
		}
		result.Blocks[record.BlockIndex] = record
	}
	if err := blockRows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) InvalidateBlocks(projectID string, blockIndices []int, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, blockIndex := range blockIndices {
		if _, err := tx.ExecContext(ctx, `DELETE FROM block_checkpoint WHERE project_id=? AND block_index=?`, projectID, blockIndex); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM stroke_component WHERE project_id=? AND block_index=?`, projectID, blockIndex); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO invalidation_log(project_id, scope, object_id, reason, created_at)
			VALUES(?,?,?,?,?)`, projectID, "block", fmt.Sprint(blockIndex), reason, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) InvalidateProject(projectID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateProjectLocked(context.Background(), projectID, reason)
}

func (s *Store) invalidateProjectLocked(ctx context.Context, projectID, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`DELETE FROM block_checkpoint WHERE project_id=?`,
		`DELETE FROM stroke_component WHERE project_id=?`,
		`DELETE FROM stroke_checkpoint WHERE project_id=?`,
		`DELETE FROM project_meta WHERE project_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, projectID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO invalidation_log(project_id, scope, object_id, reason, created_at)
		VALUES(?,?,?,?,?)`, projectID, "project", projectID, reason, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) InvalidationLog(projectID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT scope || ':' || object_id || ':' || reason FROM invalidation_log WHERE project_id=? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []string
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *Store) Counts(projectID string) (strokes, blocks int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM stroke_checkpoint WHERE project_id=?`, projectID).Scan(&strokes); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM block_checkpoint WHERE project_id=?`, projectID).Scan(&blocks); err != nil {
		return 0, 0, err
	}
	return strokes, blocks, nil
}
