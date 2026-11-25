package ocrflow

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db   *sql.DB
	path string
}

type CacheEntry struct {
	Key              string
	ChunkID          string
	ShotID           string
	LineNo           int
	Stage            string
	RegionHash       string
	RecipeVersion    string
	ParamVersion     string
	AlgorithmVersion string
	UpstreamKey      string
	Payload          []byte
	HitCount         int64
	UpdatedNS        int64
}

type CheckpointLine struct {
	LineNo           int             `json:"line_no"`
	Ordinal          int             `json:"ordinal"`
	BoxSummary       string          `json:"box_summary"`
	TextSummary      string          `json:"text_summary"`
	ContentHash      string          `json:"content_hash"`
	RecipeVersion    string          `json:"recipe_version,omitempty"`
	ParamVersion     string          `json:"param_version,omitempty"`
	AlgorithmVersion string          `json:"algorithm_version,omitempty"`
	BoxJSON          string          `json:"box_json"`
	Text             string          `json:"text"`
	ConfidenceJSON   string          `json:"confidence_json"`
	Raw              json.RawMessage `json:"raw,omitempty"`
}

type CheckpointChunk struct {
	ChunkID          string           `json:"chunk_id"`
	ShotID           string           `json:"shot_id"`
	ChunkIndex       int              `json:"chunk_index"`
	PredecessorID    string           `json:"predecessor_id"`
	ShotHash         string           `json:"shot_hash"`
	RecipeVersion    string           `json:"recipe_version"`
	ParamVersion     string           `json:"param_version"`
	AlgorithmVersion string           `json:"algorithm_version"`
	CommitOrder      int64            `json:"commit_order"`
	CommittedAtNS    int64            `json:"committed_at_ns"`
	Lines            []CheckpointLine `json:"lines"`
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = "ocrflow.db"
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." && path != ":memory:" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := path
	if path != ":memory:" {
		dsn += "?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on"
	} else {
		dsn = "file::memory:?cache=shared&_busy_timeout=5000&_foreign_keys=on"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS schema_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS cache_entries (
  cache_key TEXT PRIMARY KEY,
  chunk_id TEXT NOT NULL DEFAULT '',
  shot_id TEXT NOT NULL,
  line_no INTEGER NOT NULL,
  stage TEXT NOT NULL,
  region_hash TEXT NOT NULL,
  recipe_version TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  upstream_key TEXT NOT NULL,
  payload_json BLOB NOT NULL,
  hit_count INTEGER NOT NULL DEFAULT 0,
  updated_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cache_shot_line ON cache_entries(shot_id,line_no);
CREATE TABLE IF NOT EXISTS checkpoint_chunks (
  chunk_id TEXT PRIMARY KEY,
  shot_id TEXT NOT NULL,
  chunk_index INTEGER NOT NULL,
  predecessor_id TEXT NOT NULL DEFAULT '',
  shot_hash TEXT NOT NULL,
  recipe_version TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  line_count INTEGER NOT NULL,
  commit_order INTEGER NOT NULL UNIQUE,
  committed_at_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoint_lines (
  chunk_id TEXT NOT NULL,
  line_no INTEGER NOT NULL,
  ordinal INTEGER NOT NULL,
  shot_id TEXT NOT NULL,
  box_summary TEXT NOT NULL,
  text_summary TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  recipe_version TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  box_json TEXT NOT NULL DEFAULT '',
  text_value TEXT NOT NULL DEFAULT '',
  confidence_json TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY(chunk_id,line_no)
);
CREATE INDEX IF NOT EXISTS idx_checkpoint_lines_shot ON checkpoint_lines(shot_id,line_no);
`)
	return err
}

func (s *Store) PutCache(e CacheEntry) error {
	if e.UpdatedNS == 0 {
		e.UpdatedNS = time.Now().UnixNano()
	}
	_, err := s.db.Exec(`
INSERT INTO cache_entries(cache_key,chunk_id,shot_id,line_no,stage,region_hash,recipe_version,param_version,algorithm_version,upstream_key,payload_json,hit_count,updated_ns)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(cache_key) DO UPDATE SET
  chunk_id=excluded.chunk_id,
  region_hash=excluded.region_hash,
  recipe_version=excluded.recipe_version,
  param_version=excluded.param_version,
  algorithm_version=excluded.algorithm_version,
  upstream_key=excluded.upstream_key,
  payload_json=excluded.payload_json,
  updated_ns=excluded.updated_ns`,
		e.Key, e.ChunkID, e.ShotID, e.LineNo, e.Stage, e.RegionHash, e.RecipeVersion, e.ParamVersion, e.AlgorithmVersion, e.UpstreamKey, e.Payload, e.HitCount, e.UpdatedNS)
	return err
}

func (s *Store) GetCache(key string) (*CacheEntry, error) {
	row := s.db.QueryRow(`SELECT cache_key,chunk_id,shot_id,line_no,stage,region_hash,recipe_version,param_version,algorithm_version,upstream_key,payload_json,hit_count,updated_ns FROM cache_entries WHERE cache_key=?`, key)
	var e CacheEntry
	err := row.Scan(&e.Key, &e.ChunkID, &e.ShotID, &e.LineNo, &e.Stage, &e.RegionHash, &e.RecipeVersion, &e.ParamVersion, &e.AlgorithmVersion, &e.UpstreamKey, &e.Payload, &e.HitCount, &e.UpdatedNS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, _ = s.db.Exec(`UPDATE cache_entries SET hit_count=hit_count+1 WHERE cache_key=?`, key)
	e.HitCount++
	return &e, nil
}

func (s *Store) DeleteCache(keys []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := tx.Exec(`DELETE FROM cache_entries WHERE cache_key=?`, key); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteCacheForChunks(chunkIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, id := range chunkIDs {
		if _, err := tx.Exec(`DELETE FROM cache_entries WHERE chunk_id=?`, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListCache() ([]CacheEntry, error) {
	rows, err := s.db.Query(`SELECT cache_key,chunk_id,shot_id,line_no,stage,region_hash,recipe_version,param_version,algorithm_version,upstream_key,payload_json,hit_count,updated_ns FROM cache_entries ORDER BY shot_id,line_no,stage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheEntry
	for rows.Next() {
		var e CacheEntry
		if err := rows.Scan(&e.Key, &e.ChunkID, &e.ShotID, &e.LineNo, &e.Stage, &e.RegionHash, &e.RecipeVersion, &e.ParamVersion, &e.AlgorithmVersion, &e.UpstreamKey, &e.Payload, &e.HitCount, &e.UpdatedNS); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CommitChunk writes the chunk header and all line records in one serial
// transaction. Cache replacements join the same atomic boundary.
func (s *Store) CommitChunk(chunk CheckpointChunk, cache []CacheEntry) (int64, error) {
	sort.Slice(chunk.Lines, func(i, j int) bool {
		if chunk.Lines[i].Ordinal != chunk.Lines[j].Ordinal {
			return chunk.Lines[i].Ordinal < chunk.Lines[j].Ordinal
		}
		return chunk.Lines[i].LineNo < chunk.Lines[j].LineNo
	})
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	var nextOrder int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(commit_order),0)+1 FROM checkpoint_chunks`).Scan(&nextOrder); err != nil {
		return 0, err
	}
	if chunk.CommitOrder != 0 {
		nextOrder = chunk.CommitOrder
	}
	if chunk.CommittedAtNS == 0 {
		chunk.CommittedAtNS = time.Now().UnixNano()
	}
	_, err = tx.Exec(`
INSERT INTO checkpoint_chunks(chunk_id,shot_id,chunk_index,predecessor_id,shot_hash,recipe_version,param_version,algorithm_version,line_count,commit_order,committed_at_ns)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(chunk_id) DO UPDATE SET
  shot_id=excluded.shot_id, chunk_index=excluded.chunk_index, predecessor_id=excluded.predecessor_id,
  shot_hash=excluded.shot_hash, recipe_version=excluded.recipe_version, param_version=excluded.param_version,
  algorithm_version=excluded.algorithm_version, line_count=excluded.line_count, commit_order=excluded.commit_order,
  committed_at_ns=excluded.committed_at_ns`,
		chunk.ChunkID, chunk.ShotID, chunk.ChunkIndex, chunk.PredecessorID, chunk.ShotHash, chunk.RecipeVersion, chunk.ParamVersion, chunk.AlgorithmVersion, len(chunk.Lines), nextOrder, chunk.CommittedAtNS)
	if err != nil {
		return 0, err
	}
	for _, line := range chunk.Lines {
		_, err = tx.Exec(`
INSERT INTO checkpoint_lines(chunk_id,line_no,ordinal,shot_id,box_summary,text_summary,content_hash,recipe_version,param_version,algorithm_version,box_json,text_value,confidence_json)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(chunk_id,line_no) DO UPDATE SET
  ordinal=excluded.ordinal, shot_id=excluded.shot_id, box_summary=excluded.box_summary, text_summary=excluded.text_summary,
  content_hash=excluded.content_hash, recipe_version=excluded.recipe_version, param_version=excluded.param_version,
  algorithm_version=excluded.algorithm_version, box_json=excluded.box_json, text_value=excluded.text_value,
  confidence_json=excluded.confidence_json`,
			chunk.ChunkID, line.LineNo, line.Ordinal, chunk.ShotID, line.BoxSummary, line.TextSummary, line.ContentHash, chunk.RecipeVersion, chunk.ParamVersion, chunk.AlgorithmVersion, line.BoxJSON, line.Text, line.ConfidenceJSON)
		if err != nil {
			return 0, err
		}
	}
	for _, e := range cache {
		if e.UpdatedNS == 0 {
			e.UpdatedNS = chunk.CommittedAtNS
		}
		_, err = tx.Exec(`
INSERT INTO cache_entries(cache_key,chunk_id,shot_id,line_no,stage,region_hash,recipe_version,param_version,algorithm_version,upstream_key,payload_json,hit_count,updated_ns)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(cache_key) DO UPDATE SET
  chunk_id=excluded.chunk_id, region_hash=excluded.region_hash, recipe_version=excluded.recipe_version,
  param_version=excluded.param_version, algorithm_version=excluded.algorithm_version, upstream_key=excluded.upstream_key,
  payload_json=excluded.payload_json, updated_ns=excluded.updated_ns`,
			e.Key, e.ChunkID, e.ShotID, e.LineNo, e.Stage, e.RegionHash, e.RecipeVersion, e.ParamVersion, e.AlgorithmVersion, e.UpstreamKey, e.Payload, e.HitCount, e.UpdatedNS)
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	tx = nil
	return nextOrder, nil
}

func (s *Store) ListCheckpoints() ([]CheckpointChunk, error) {
	rows, err := s.db.Query(`SELECT chunk_id,shot_id,chunk_index,predecessor_id,shot_hash,recipe_version,param_version,algorithm_version,commit_order,committed_at_ns FROM checkpoint_chunks ORDER BY commit_order,chunk_index,chunk_id`)
	if err != nil {
		return nil, err
	}
	var chunks []CheckpointChunk
	for rows.Next() {
		var c CheckpointChunk
		if err := rows.Scan(&c.ChunkID, &c.ShotID, &c.ChunkIndex, &c.PredecessorID, &c.ShotHash, &c.RecipeVersion, &c.ParamVersion, &c.AlgorithmVersion, &c.CommitOrder, &c.CommittedAtNS); err != nil {
			_ = rows.Close()
			return nil, err
		}
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range chunks {
		c := &chunks[i]
		lrows, err := s.db.Query(`SELECT line_no,ordinal,box_summary,text_summary,content_hash,recipe_version,param_version,algorithm_version,box_json,text_value,confidence_json FROM checkpoint_lines WHERE chunk_id=? ORDER BY ordinal,line_no`, c.ChunkID)
		if err != nil {
			return nil, err
		}
		for lrows.Next() {
			var l CheckpointLine
			if err := lrows.Scan(&l.LineNo, &l.Ordinal, &l.BoxSummary, &l.TextSummary, &l.ContentHash, &l.RecipeVersion, &l.ParamVersion, &l.AlgorithmVersion, &l.BoxJSON, &l.Text, &l.ConfidenceJSON); err != nil {
				_ = lrows.Close()
				return nil, err
			}
			c.Lines = append(c.Lines, l)
		}
		if err := lrows.Err(); err != nil {
			_ = lrows.Close()
			return nil, err
		}
		if err := lrows.Close(); err != nil {
			return nil, err
		}
	}
	return chunks, nil
}

type ChunkExpectation struct {
	ChunkID          string
	ShotID           string
	PredecessorID    string
	ShotHash         string
	RecipeVersion    string
	ParamVersion     string
	AlgorithmVersion string
	LineHashes       map[int]string
}

type RecoveryReport struct {
	CheckedChunks       []string `json:"checked_chunks"`
	ValidChunks         []string `json:"valid_chunks"`
	DiscardedChunks     []string `json:"discarded_chunks"`
	ResumeAfterChunkID  string   `json:"resume_after_chunk_id"`
	InvalidChunkID      string   `json:"invalid_chunk_id,omitempty"`
	Reason              string   `json:"reason,omitempty"`
	KilledChunkReported bool     `json:"killed_chunk_reported"`
}

// Recover validates the contiguous committed prefix. Any hash/version mismatch
// discards that chunk and every later chained chunk. An uncommitted killed
// transaction is absent and deliberately has no exit report.
func (s *Store) Recover(expected []ChunkExpectation) (*RecoveryReport, error) {
	chunks, err := s.ListCheckpoints()
	if err != nil {
		return nil, err
	}
	byID := map[string]ChunkExpectation{}
	for _, e := range expected {
		byID[e.ChunkID] = e
	}
	rep := &RecoveryReport{KilledChunkReported: false}
	valid := map[string]bool{}
	var firstBad *CheckpointChunk
	badReason := ""
	for i := range chunks {
		c := chunks[i]
		rep.CheckedChunks = append(rep.CheckedChunks, c.ChunkID)
		reason := ""
		exp, hasExp := byID[c.ChunkID]
		if hasExp {
			if exp.ShotHash != "" && c.ShotHash != exp.ShotHash {
				reason = fmt.Sprintf("shot content hash mismatch: stored=%s expected=%s", c.ShotHash[:12], exp.ShotHash[:12])
			} else if exp.ParamVersion != "" && c.ParamVersion != exp.ParamVersion {
				reason = fmt.Sprintf("param version mismatch: stored=%s expected=%s", c.ParamVersion, exp.ParamVersion)
			} else if exp.AlgorithmVersion != "" && c.AlgorithmVersion != exp.AlgorithmVersion {
				reason = fmt.Sprintf("algorithm version mismatch: stored=%s expected=%s", c.AlgorithmVersion, exp.AlgorithmVersion)
			} else if exp.RecipeVersion != "" && c.RecipeVersion != exp.RecipeVersion {
				reason = fmt.Sprintf("recipe version mismatch: stored=%s expected=%s", c.RecipeVersion, exp.RecipeVersion)
			} else if exp.PredecessorID != c.PredecessorID {
				reason = fmt.Sprintf("predecessor mismatch: stored=%s expected=%s", c.PredecessorID, exp.PredecessorID)
			} else {
				for _, l := range c.Lines {
					if hv, ok := exp.LineHashes[l.LineNo]; ok && hv != l.ContentHash {
						reason = fmt.Sprintf("line %d content hash mismatch", l.LineNo)
						break
					}
					if l.ParamVersion != c.ParamVersion || l.AlgorithmVersion != c.AlgorithmVersion || l.RecipeVersion != c.RecipeVersion {
						reason = fmt.Sprintf("line %d version column mismatch", l.LineNo)
						break
					}
				}
			}
		}
		if reason == "" && i > 0 && c.PredecessorID != chunks[i-1].ChunkID {
			reason = "broken predecessor chain"
		}
		if reason != "" {
			firstBad = &chunks[i]
			badReason = reason
			break
		}
		valid[c.ChunkID] = true
		rep.ValidChunks = append(rep.ValidChunks, c.ChunkID)
		rep.ResumeAfterChunkID = c.ChunkID
	}
	if firstBad == nil {
		return rep, nil
	}
	discard := make([]string, 0)
	for _, c := range chunks {
		if c.CommitOrder >= firstBad.CommitOrder {
			discard = append(discard, c.ChunkID)
		}
	}
	if err := s.deleteChunks(discard); err != nil {
		return nil, err
	}
	if err := s.DeleteCacheForChunks(discard); err != nil {
		return nil, err
	}
	rep.DiscardedChunks = discard
	rep.InvalidChunkID = firstBad.ChunkID
	rep.Reason = badReason
	if len(rep.ValidChunks) > 0 {
		rep.ResumeAfterChunkID = rep.ValidChunks[len(rep.ValidChunks)-1]
	} else {
		rep.ResumeAfterChunkID = firstBad.PredecessorID
	}
	return rep, nil
}

func (s *Store) deleteChunks(ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM checkpoint_lines WHERE chunk_id=?`, id); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`DELETE FROM checkpoint_chunks WHERE chunk_id=?`, id); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// debugInsertKilledTransaction simulates a hard kill after partial writes and
// before commit. The returned closer rolls back when the test process does not
// actually exit; a real killed process leaves SQLite to roll back the WAL.
func (s *Store) debugInsertKilledTransaction(chunk CheckpointChunk) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO checkpoint_chunks(chunk_id,shot_id,chunk_index,predecessor_id,shot_hash,recipe_version,param_version,algorithm_version,line_count,commit_order,committed_at_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		chunk.ChunkID, chunk.ShotID, chunk.ChunkIndex, chunk.PredecessorID, chunk.ShotHash, chunk.RecipeVersion, chunk.ParamVersion, chunk.AlgorithmVersion, len(chunk.Lines), 999999, time.Now().UnixNano())
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if len(chunk.Lines) > 0 {
		l := chunk.Lines[0]
		_, err = tx.Exec(`INSERT INTO checkpoint_lines(chunk_id,line_no,ordinal,shot_id,box_summary,text_summary,content_hash,recipe_version,param_version,algorithm_version) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			chunk.ChunkID, l.LineNo, l.Ordinal, chunk.ShotID, l.BoxSummary, l.TextSummary, l.ContentHash, chunk.RecipeVersion, chunk.ParamVersion, chunk.AlgorithmVersion)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Rollback()
}

func TextSummary(text string) string { return "textsha256:" + HashString(text) }

func ConfidenceSummary(chars []CharHit) string {
	data, _ := json.Marshal(chars)
	return string(data)
}
