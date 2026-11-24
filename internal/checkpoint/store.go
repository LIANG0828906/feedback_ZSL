package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"iconbench/internal/model"
)

const (
	StatePending    = "pending"
	StateProcessing = "processing"
	StateCommitted  = "committed"
	StateFailed     = "failed"

	LayerGradient   = "gradient"
	LayerMarkers    = "markers"
	LayerFloodMerge = "flood_merge"
	LayerStats      = "stats"
)

var Layers = []string{LayerGradient, LayerMarkers, LayerFloodMerge, LayerStats}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type LayerRecord struct {
	ImageID          string
	BlockIndex       int
	BlockX           int
	BlockY           int
	Layer            string
	State            string
	ArtifactJSON     string
	SummaryJSON      string
	RAGJSON          string
	ContentHash      string
	ParamVersion     int64
	ParamHash        string
	AlgorithmVersion string
	SchemaVersion    string
	CommitSeq        int64
}

type CommitRecord struct {
	Seq              int64  `json:"seq"`
	ImageID          string `json:"image_id"`
	BlockIndex       int    `json:"block_index"`
	BlockX           int    `json:"block_x"`
	BlockY           int    `json:"block_y"`
	Layer            string `json:"layer"`
	ContentHash      string `json:"content_hash"`
	ParamVersion     int64  `json:"param_version"`
	ParamHash        string `json:"param_hash"`
	AlgorithmVersion string `json:"algorithm_version"`
}

type Violation struct {
	BlockX   int    `json:"block_x"`
	BlockY   int    `json:"block_y"`
	Layer    string `json:"layer"`
	Field    string `json:"field"`
	Expected any    `json:"expected"`
	Actual   any    `json:"actual"`
}

func Open(path string) (*Store, error) {
	dsn := filepath.ToSlash(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS checkpoints (
  image_id TEXT NOT NULL,
  block_index INTEGER NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  layer TEXT NOT NULL,
  state TEXT NOT NULL,
  artifact_json TEXT NOT NULL DEFAULT '',
  summary_json TEXT NOT NULL DEFAULT '',
  rag_json TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  param_version INTEGER NOT NULL DEFAULT 0,
  param_hash TEXT NOT NULL DEFAULT '',
  algorithm_version TEXT NOT NULL DEFAULT '',
  schema_version TEXT NOT NULL DEFAULT '',
  commit_seq INTEGER,
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY(image_id, block_index, layer)
);
CREATE TABLE IF NOT EXISTS layer_commits (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  image_id TEXT NOT NULL,
  block_index INTEGER NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  layer TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_version INTEGER NOT NULL,
  param_hash TEXT NOT NULL,
  algorithm_version TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS state_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  image_id TEXT NOT NULL,
  block_index INTEGER NOT NULL,
  layer TEXT NOT NULL,
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  ok INTEGER NOT NULL,
  message TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS image_results (
  image_id TEXT PRIMARY KEY,
  param_version INTEGER NOT NULL,
  mask_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS judgments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  image_id TEXT NOT NULL,
  param_version INTEGER NOT NULL,
  ok INTEGER NOT NULL,
  code TEXT NOT NULL DEFAULT '',
  tolerance_json TEXT NOT NULL,
  metrics_json TEXT NOT NULL,
  detail_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`)
	return err
}

func CanTransition(from, to string) bool {
	if from == StatePending && to == StateProcessing {
		return true
	}
	if from == StateProcessing && (to == StateCommitted || to == StateFailed) {
		return true
	}
	if from == StateFailed && to == StatePending {
		return true
	}
	return false
}

func (s *Store) InitImage(imageID string, blockCount int, tileSize, width, height int, p model.Params) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for bi := 0; bi < blockCount; bi++ {
		bx, by := blockXY(bi, width, height, tileSize)
		for _, layer := range Layers {
			_, err := tx.Exec(`
INSERT INTO checkpoints(image_id,block_index,block_x,block_y,layer,state,param_version,param_hash,algorithm_version,schema_version)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(image_id,block_index,layer) DO NOTHING`,
				imageID, bi, bx, by, layer, StatePending, p.ParamVersion, p.CanonicalHash(), model.AlgorithmVersion, model.SchemaVersion)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func blockXY(index, width, height, tile int) (int, int) {
	cols := (width + tile - 1) / tile
	if cols <= 0 {
		cols = 1
	}
	return index % cols, index / cols
}

func (s *Store) GetState(imageID string, blockIndex int, layer string) (string, error) {
	var state string
	err := s.db.QueryRow(`SELECT state FROM checkpoints WHERE image_id=? AND block_index=? AND layer=?`, imageID, blockIndex, layer).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", &model.Error{Code: model.CodeNotFound, Message: "checkpoint layer not initialized", Layer: layer}
	}
	return state, err
}

func (s *Store) Transition(ctx context.Context, imageID string, blockIndex int, layer, from, to string) error {
	if !CanTransition(from, to) {
		current, _ := s.GetState(imageID, blockIndex, layer)
		return &model.Error{
			Code:     model.CodeState,
			Message:  fmt.Sprintf("illegal state transition %s -> %s for block %d layer %s", from, to, blockIndex, layer),
			Layer:    layer,
			BlockX:   blockIndex,
			Expected: from,
			Actual:   current,
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var current string
	err = tx.QueryRow(`SELECT state FROM checkpoints WHERE image_id=? AND block_index=? AND layer=?`, imageID, blockIndex, layer).Scan(&current)
	if err != nil {
		return err
	}
	if current != from {
		_, _ = tx.Exec(`INSERT INTO state_events(image_id,block_index,layer,from_state,to_state,ok,message) VALUES(?,?,?,?,?,0,?)`,
			imageID, blockIndex, layer, current, to, "expected state "+from)
		if err := tx.Commit(); err != nil {
			return err
		}
		return &model.Error{Code: model.CodeState, Layer: layer, BlockX: blockIndex, Expected: from, Actual: current,
			Message: fmt.Sprintf("state mismatch: expected %s, found %s", from, current)}
	}
	res, err := tx.Exec(`UPDATE checkpoints SET state=?, updated_at=datetime('now') WHERE image_id=? AND block_index=? AND layer=? AND state=?`,
		to, imageID, blockIndex, layer, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return &model.Error{Code: model.CodeState, Layer: layer, BlockX: blockIndex, Expected: from, Actual: "concurrent-update", Message: "state update did not affect one row"}
	}
	_, err = tx.Exec(`INSERT INTO state_events(image_id,block_index,layer,from_state,to_state,ok) VALUES(?,?,?,?,?,1)`,
		imageID, blockIndex, layer, from, to)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitLayer(ctx context.Context, rec LayerRecord) error {
	if !CanTransition(StateProcessing, StateCommitted) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	err = tx.QueryRow(`SELECT state FROM checkpoints WHERE image_id=? AND block_index=? AND layer=?`, rec.ImageID, rec.BlockIndex, rec.Layer).Scan(&current)
	if err != nil {
		return err
	}
	if current != StateProcessing {
		return &model.Error{Code: model.CodeState, Layer: rec.Layer, BlockX: rec.BlockX, BlockY: rec.BlockY, Expected: StateProcessing, Actual: current, Message: "commit requires processing state"}
	}
	res, err := tx.Exec(`
INSERT INTO layer_commits(image_id,block_index,block_x,block_y,layer,content_hash,param_version,param_hash,algorithm_version)
VALUES(?,?,?,?,?,?,?,?,?)`,
		rec.ImageID, rec.BlockIndex, rec.BlockX, rec.BlockY, rec.Layer, rec.ContentHash, rec.ParamVersion, rec.ParamHash, rec.AlgorithmVersion)
	if err != nil {
		return err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
UPDATE checkpoints SET state=?, artifact_json=?, summary_json=?, rag_json=?, content_hash=?,
 param_version=?, param_hash=?, algorithm_version=?, schema_version=?, commit_seq=?, updated_at=datetime('now')
WHERE image_id=? AND block_index=? AND layer=?`,
		StateCommitted, rec.ArtifactJSON, rec.SummaryJSON, rec.RAGJSON, rec.ContentHash,
		rec.ParamVersion, rec.ParamHash, rec.AlgorithmVersion, rec.SchemaVersion, seq,
		rec.ImageID, rec.BlockIndex, rec.Layer)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO state_events(image_id,block_index,layer,from_state,to_state,ok) VALUES(?,?,?,?,?,1)`,
		rec.ImageID, rec.BlockIndex, rec.Layer, StateProcessing, StateCommitted)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailLayer(ctx context.Context, imageID string, blockIndex int, layer string, reason string) error {
	if err := s.Transition(ctx, imageID, blockIndex, layer, StateProcessing, StateFailed); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO state_events(image_id,block_index,layer,from_state,to_state,ok,message) VALUES(?,?,?,?,?,0,?)`,
		imageID, blockIndex, layer, StateProcessing, StateFailed, reason)
	return err
}

func (s *Store) ResetInterrupted(ctx context.Context, imageID string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT block_index, layer FROM checkpoints WHERE image_id=? AND state=? ORDER BY block_index, layer`, imageID, StateProcessing)
	if err != nil {
		return 0, err
	}
	var ids []struct {
		block int
		layer string
	}
	for rows.Next() {
		var block int
		var layer string
		if err := rows.Scan(&block, &layer); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, struct {
			block int
			layer string
		}{block, layer})
	}
	rows.Close()
	for _, id := range ids {
		if err := s.Transition(ctx, imageID, id.block, id.layer, StateProcessing, StateFailed); err != nil {
			return 0, err
		}
		if err := s.Transition(ctx, imageID, id.block, id.layer, StateFailed, StatePending); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func (s *Store) ResetImage(ctx context.Context, imageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM checkpoints WHERE image_id=?`, imageID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM layer_commits WHERE image_id=?`, imageID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM image_results WHERE image_id=?`, imageID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommittedLayers(ctx context.Context, imageID string) (map[string]LayerRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT image_id,block_index,block_x,block_y,layer,state,artifact_json,summary_json,rag_json,content_hash,
       param_version,param_hash,algorithm_version,schema_version,COALESCE(commit_seq,0)
FROM checkpoints WHERE image_id=? AND state=? ORDER BY block_index,
  CASE layer WHEN 'gradient' THEN 0 WHEN 'markers' THEN 1 WHEN 'flood_merge' THEN 2 WHEN 'stats' THEN 3 ELSE 4 END`,
		imageID, StateCommitted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]LayerRecord{}
	for rows.Next() {
		var r LayerRecord
		if err := rows.Scan(&r.ImageID, &r.BlockIndex, &r.BlockX, &r.BlockY, &r.Layer, &r.State, &r.ArtifactJSON, &r.SummaryJSON, &r.RAGJSON, &r.ContentHash,
			&r.ParamVersion, &r.ParamHash, &r.AlgorithmVersion, &r.SchemaVersion, &r.CommitSeq); err != nil {
			return nil, err
		}
		out[Key(r.BlockIndex, r.Layer)] = r
	}
	return out, rows.Err()
}

func (s *Store) Sequence(ctx context.Context, imageID string) ([]CommitRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,image_id,block_index,block_x,block_y,layer,content_hash,param_version,param_hash,algorithm_version FROM layer_commits WHERE image_id=? ORDER BY seq`, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitRecord
	for rows.Next() {
		var r CommitRecord
		if err := rows.Scan(&r.Seq, &r.ImageID, &r.BlockIndex, &r.BlockX, &r.BlockY, &r.Layer, &r.ContentHash, &r.ParamVersion, &r.ParamHash, &r.AlgorithmVersion); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func Key(blockIndex int, layer string) string {
	return fmt.Sprintf("%d:%s", blockIndex, layer)
}

func (s *Store) SaveResult(ctx context.Context, imageID string, paramVersion int64, maskHash, resultJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO image_results(image_id,param_version,mask_hash,result_json) VALUES(?,?,?,?)
ON CONFLICT(image_id) DO UPDATE SET param_version=excluded.param_version, mask_hash=excluded.mask_hash,
 result_json=excluded.result_json, updated_at=datetime('now')`, imageID, paramVersion, maskHash, resultJSON)
	return err
}

func (s *Store) LoadResult(ctx context.Context, imageID string) (int64, string, string, error) {
	var v int64
	var hash, js string
	err := s.db.QueryRowContext(ctx, `SELECT param_version,mask_hash,result_json FROM image_results WHERE image_id=?`, imageID).Scan(&v, &hash, &js)
	return v, hash, js, err
}

func (s *Store) InsertJudgment(ctx context.Context, j model.Judgment, detailJSON string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tol, _ := json.Marshal(j.Tolerances)
	met, _ := json.Marshal(j.Metrics)
	res, err := s.db.ExecContext(ctx, `INSERT INTO judgments(image_id,param_version,ok,code,tolerance_json,metrics_json,detail_json) VALUES(?,?,?,?,?,?,?)`,
		j.ImageID, j.ParamVersion, boolToInt(j.OK), j.Code, string(tol), string(met), detailJSON)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListJudgments(ctx context.Context, imageID string) ([]model.Judgment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,image_id,param_version,ok,code,tolerance_json,metrics_json FROM judgments WHERE (?='' OR image_id=?) ORDER BY id`, imageID, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Judgment
	for rows.Next() {
		var j model.Judgment
		var ok int
		var tol, met string
		if err := rows.Scan(&j.ID, &j.ImageID, &j.ParamVersion, &ok, &j.Code, &tol, &met); err != nil {
			return nil, err
		}
		j.OK = ok == 1
		_ = json.Unmarshal([]byte(tol), &j.Tolerances)
		_ = json.Unmarshal([]byte(met), &j.Metrics)
		out = append(out, j)
	}
	return out, rows.Err()
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
