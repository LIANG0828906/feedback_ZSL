package lipsync

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
	db       *sql.DB
	path     string
	commitMu sync.Mutex
}

type ShardCheckpoint struct {
	ShardCommitRecord
	PackSummaryJSON  string `json:"packSummaryJson"`
	FrameRecordsJSON string `json:"frameRecordsJson"`
	DispatchJSON     string `json:"dispatchJson"`
	ResultJSON       string `json:"resultJson"`
}

type RunRecord struct {
	RunID      string `json:"runId"`
	ProjectID  string `json:"projectId"`
	Status     string `json:"status"`
	StartedAt  string `json:"startedAt"`
	EndedAt    string `json:"endedAt,omitempty"`
	ReportJSON string `json:"reportJson,omitempty"`
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS shard_commits (
  commit_seq INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  shard_index INTEGER NOT NULL,
  revision INTEGER NOT NULL DEFAULT 0,
  dialogue_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  recipe_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  pack_count INTEGER NOT NULL,
  frame_count INTEGER NOT NULL,
  committed_at TEXT NOT NULL,
  pack_summary_json TEXT NOT NULL,
  frame_records_json TEXT NOT NULL,
  dispatch_json TEXT NOT NULL,
  result_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_shard_commits_project_shard
  ON shard_commits(project_id, shard_index, commit_seq);

CREATE TABLE IF NOT EXISTS cache_entries (
  cache_key TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  dialogue_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  recipe_version TEXT NOT NULL,
  upstream_keys_json TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  revision INTEGER NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cache_project_dialogue
  ON cache_entries(project_id, dialogue_id);

CREATE TABLE IF NOT EXISTS runs (
  run_id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  status TEXT NOT NULL,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  report_json TEXT
);
`

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = ":memory:"
	}
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create database directory: %w", err)
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) StartRun(runID, projectID string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO runs(run_id,project_id,status,started_at,ended_at,report_json)
		 VALUES(?,?,?,?,NULL,NULL)`,
		runID, projectID, "running", nowRFC3339(),
	)
	return err
}

func (s *Store) FinishRun(runID, status, reportJSON string) error {
	_, err := s.db.Exec(
		`UPDATE runs SET status=?, ended_at=?, report_json=? WHERE run_id=?`,
		status, nowRFC3339(), reportJSON, runID,
	)
	return err
}

func (s *Store) LatestRun(projectID string) (*RunRecord, error) {
	row := s.db.QueryRow(
		`SELECT run_id,project_id,status,started_at,COALESCE(ended_at,''),COALESCE(report_json,'')
		 FROM runs WHERE project_id=? ORDER BY started_at DESC, rowid DESC LIMIT 1`,
		projectID,
	)
	var record RunRecord
	if err := row.Scan(&record.RunID, &record.ProjectID, &record.Status,
		&record.StartedAt, &record.EndedAt, &record.ReportJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

func (s *Store) CommitShard(checkpoint *ShardCheckpoint, entries []CacheEntry) (int64, error) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	committedAt := checkpoint.CommittedAt
	if committedAt == "" {
		committedAt = nowRFC3339()
	}
	res, err := tx.Exec(
		`INSERT INTO shard_commits(
			run_id,project_id,shard_index,revision,dialogue_id,content_hash,
			recipe_version,algorithm_version,pack_count,frame_count,committed_at,
			pack_summary_json,frame_records_json,dispatch_json,result_json
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		checkpoint.RunID, checkpoint.ProjectID, checkpoint.ShardIndex, checkpoint.Revision,
		checkpoint.DialogueID, checkpoint.ContentHash, checkpoint.RecipeVersion,
		checkpoint.AlgorithmVersion, checkpoint.PackCount, checkpoint.FrameCount, committedAt,
		checkpoint.PackSummaryJSON, checkpoint.FrameRecordsJSON,
		checkpoint.DispatchJSON, checkpoint.ResultJSON,
	)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	checkpoint.CommitSeq = seq
	checkpoint.CommittedAt = committedAt

	for _, entry := range entries {
		upstream, err := json.Marshal(entry.UpstreamKeys)
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(
			`INSERT INTO cache_entries(
				cache_key,project_id,dialogue_id,kind,content_hash,recipe_version,
				upstream_keys_json,payload_json,revision,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(cache_key) DO UPDATE SET
				dialogue_id=excluded.dialogue_id,
				kind=excluded.kind,
				content_hash=excluded.content_hash,
				recipe_version=excluded.recipe_version,
				upstream_keys_json=excluded.upstream_keys_json,
				payload_json=excluded.payload_json,
				revision=excluded.revision,
				updated_at=excluded.updated_at`,
			entry.Key, entry.ProjectID, entry.DialogueID, entry.Kind,
			entry.ContentHash, entry.RecipeVersion, string(upstream),
			entry.PayloadJSON, entry.Revision, committedAt,
		)
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

func (s *Store) ListCommitRecords(projectID string) ([]ShardCommitRecord, error) {
	rows, err := s.db.Query(
		`SELECT commit_seq,run_id,project_id,shard_index,revision,dialogue_id,content_hash,
		        recipe_version,algorithm_version,pack_count,frame_count,committed_at
		 FROM shard_commits WHERE project_id=? ORDER BY commit_seq ASC`, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShardCommitRecord{}
	for rows.Next() {
		var rec ShardCommitRecord
		if err := rows.Scan(&rec.CommitSeq, &rec.RunID, &rec.ProjectID, &rec.ShardIndex,
			&rec.Revision, &rec.DialogueID, &rec.ContentHash, &rec.RecipeVersion,
			&rec.AlgorithmVersion, &rec.PackCount, &rec.FrameCount, &rec.CommittedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) LatestShards(projectID string) ([]ShardCheckpoint, error) {
	rows, err := s.db.Query(
		`SELECT commit_seq,run_id,project_id,shard_index,revision,dialogue_id,content_hash,
		        recipe_version,algorithm_version,pack_count,frame_count,committed_at,
		        pack_summary_json,frame_records_json,dispatch_json,result_json
		 FROM shard_commits
		 WHERE project_id=? AND commit_seq IN (
		   SELECT MAX(commit_seq) FROM shard_commits WHERE project_id=? GROUP BY shard_index
		 )
		 ORDER BY shard_index ASC`,
		projectID, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShardCheckpoint{}
	for rows.Next() {
		var cp ShardCheckpoint
		if err := rows.Scan(&cp.CommitSeq, &cp.RunID, &cp.ProjectID, &cp.ShardIndex,
			&cp.Revision, &cp.DialogueID, &cp.ContentHash, &cp.RecipeVersion,
			&cp.AlgorithmVersion, &cp.PackCount, &cp.FrameCount, &cp.CommittedAt,
			&cp.PackSummaryJSON, &cp.FrameRecordsJSON, &cp.DispatchJSON, &cp.ResultJSON); err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

func (s *Store) InvalidateShard(projectID string, shardIndex int, dialogueID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM shard_commits WHERE project_id=? AND shard_index=?`, projectID, shardIndex); err != nil {
		return err
	}
	if dialogueID != "" {
		if _, err := tx.Exec(`DELETE FROM cache_entries WHERE project_id=? AND dialogue_id=?`, projectID, dialogueID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpsertCacheEntry(entry CacheEntry) error {
	upstream, err := json.Marshal(entry.UpstreamKeys)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO cache_entries(
			cache_key,project_id,dialogue_id,kind,content_hash,recipe_version,
			upstream_keys_json,payload_json,revision,updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(cache_key) DO UPDATE SET
			project_id=excluded.project_id,
			dialogue_id=excluded.dialogue_id,
			kind=excluded.kind,
			content_hash=excluded.content_hash,
			recipe_version=excluded.recipe_version,
			upstream_keys_json=excluded.upstream_keys_json,
			payload_json=excluded.payload_json,
			revision=excluded.revision,
			updated_at=excluded.updated_at`,
		entry.Key, entry.ProjectID, entry.DialogueID, entry.Kind,
		entry.ContentHash, entry.RecipeVersion, string(upstream),
		entry.PayloadJSON, entry.Revision, nowRFC3339(),
	)
	return err
}

func (s *Store) LoadCacheEntries(projectID string) ([]CacheEntry, error) {
	rows, err := s.db.Query(
		`SELECT cache_key,project_id,dialogue_id,kind,content_hash,recipe_version,
		        upstream_keys_json,payload_json,revision
		 FROM cache_entries WHERE project_id=?`, projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CacheEntry{}
	for rows.Next() {
		var entry CacheEntry
		var upstream string
		if err := rows.Scan(&entry.Key, &entry.ProjectID, &entry.DialogueID, &entry.Kind,
			&entry.ContentHash, &entry.RecipeVersion, &upstream, &entry.PayloadJSON, &entry.Revision); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(upstream), &entry.UpstreamKeys); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
