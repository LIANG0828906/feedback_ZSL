package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"storyboardstudio/internal/model"
)

type NodeSummary struct {
	LastNodeID string                       `json:"last_node_id"`
	EventOrder []string                     `json:"event_order"`
	Arrived    map[string]bool              `json:"arrived"`
	Characters map[string]map[string]string `json:"characters"`
}

type Checkpoint struct {
	SessionID            string          `json:"session_id"`
	ShotGlobalIndex      int             `json:"shot_global_index"`
	NodeID               string          `json:"node_id"`
	ShotLocalIndex       int             `json:"shot_local_index"`
	ConsumedChoiceOffset int             `json:"consumed_choice_offset"`
	CursorMS             int64           `json:"cursor_ms"`
	Flags                map[string]bool `json:"flags"`
	Summary              NodeSummary     `json:"node_summary"`
	ShotHash             string          `json:"shot_hash"`
	ContentHash          string          `json:"content_hash"`
	StructureVersion     string          `json:"structure_version"`
	AlgorithmVersion     string          `json:"algorithm_version"`
}

type SessionRecord struct {
	ID               string `json:"id"`
	ProjectID        string `json:"project_id"`
	ContentHash      string `json:"content_hash"`
	StructureVersion string `json:"structure_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	Status           string `json:"status"`
	ResultJSON       string `json:"result_json,omitempty"`
	Revision         int64  `json:"revision"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	dsn := filepath.ToSlash(path)
	if path != ":memory:" {
		dsn = dsn + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS checkpoints (
  session_id TEXT NOT NULL,
  shot_global_index INTEGER NOT NULL,
  node_id TEXT NOT NULL,
  shot_local_index INTEGER NOT NULL,
  consumed_choice_offset INTEGER NOT NULL,
  cursor_ms INTEGER NOT NULL,
  flags_json TEXT NOT NULL,
  node_summary_json TEXT NOT NULL,
  shot_hash TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL,
  structure_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  write_seq INTEGER NOT NULL,
  PRIMARY KEY (session_id, shot_global_index)
) STRICT;
CREATE TABLE IF NOT EXISTS shot_cache (
  cache_key TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  shot_global_index INTEGER NOT NULL,
  node_id TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  payload_json TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  structure_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  status TEXT NOT NULL,
  result_json TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE TABLE IF NOT EXISTS write_log (
  write_seq INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  shot_global_index INTEGER NOT NULL,
  kind TEXT NOT NULL
);
`)
	return err
}

func (s *Store) CommitBoundary(cp Checkpoint, shot model.PresentedShot) error {
	flagsJSON, err := json.Marshal(cp.Flags)
	if err != nil {
		return err
	}
	summaryJSON, err := json.Marshal(cp.Summary)
	if err != nil {
		return err
	}
	payloadJSON, err := json.Marshal(shot)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(write_seq), 0) + 1 FROM write_log`).Scan(&nextSeq); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO write_log(write_seq, session_id, shot_global_index, kind) VALUES(?, ?, ?, 'boundary')`,
		nextSeq, cp.SessionID, cp.ShotGlobalIndex); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO checkpoints(session_id, shot_global_index, node_id, shot_local_index, consumed_choice_offset,
  cursor_ms, flags_json, node_summary_json, shot_hash, content_hash, structure_version, algorithm_version, write_seq)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, shot_global_index) DO UPDATE SET
  node_id=excluded.node_id, shot_local_index=excluded.shot_local_index,
  consumed_choice_offset=excluded.consumed_choice_offset, cursor_ms=excluded.cursor_ms,
  flags_json=excluded.flags_json, node_summary_json=excluded.node_summary_json,
  shot_hash=excluded.shot_hash, content_hash=excluded.content_hash, structure_version=excluded.structure_version,
  algorithm_version=excluded.algorithm_version, write_seq=excluded.write_seq`,
		cp.SessionID, cp.ShotGlobalIndex, cp.NodeID, cp.ShotLocalIndex, cp.ConsumedChoiceOffset,
		cp.CursorMS, string(flagsJSON), string(summaryJSON), cp.ShotHash, cp.ContentHash, cp.StructureVersion,
		cp.AlgorithmVersion, nextSeq); err != nil {
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO shot_cache(cache_key, project_id, shot_global_index, node_id, algorithm_version, payload_json)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(cache_key) DO UPDATE SET
  project_id=excluded.project_id, shot_global_index=excluded.shot_global_index,
  node_id=excluded.node_id, algorithm_version=excluded.algorithm_version,
  payload_json=excluded.payload_json`,
		shot.ContentHash, "", shot.GlobalIndex, shot.NodeID, model.AlgorithmVersion, string(payloadJSON)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PutShot(projectID string, shot model.PresentedShot) error {
	payloadJSON, err := json.Marshal(shot)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`
INSERT INTO shot_cache(cache_key, project_id, shot_global_index, node_id, algorithm_version, payload_json)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(cache_key) DO UPDATE SET payload_json=excluded.payload_json`,
		shot.ContentHash, projectID, shot.GlobalIndex, shot.NodeID, model.AlgorithmVersion, string(payloadJSON))
	return err
}

func (s *Store) GetShot(cacheKey string) (*model.PresentedShot, bool, error) {
	var payload string
	err := s.db.QueryRow(`SELECT payload_json FROM shot_cache WHERE cache_key = ? AND algorithm_version = ?`,
		cacheKey, model.AlgorithmVersion).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var shot model.PresentedShot
	if err := json.Unmarshal([]byte(payload), &shot); err != nil {
		return nil, false, err
	}
	return &shot, true, nil
}

func (s *Store) Latest(sessionID string) (*Checkpoint, error) {
	row := s.db.QueryRow(`
SELECT session_id, shot_global_index, node_id, shot_local_index, consumed_choice_offset, cursor_ms,
       flags_json, node_summary_json, shot_hash, content_hash, structure_version, algorithm_version
FROM checkpoints WHERE session_id = ? ORDER BY shot_global_index DESC LIMIT 1`, sessionID)
	var cp Checkpoint
	var flagsJSON, summaryJSON string
	err := row.Scan(&cp.SessionID, &cp.ShotGlobalIndex, &cp.NodeID, &cp.ShotLocalIndex,
		&cp.ConsumedChoiceOffset, &cp.CursorMS, &flagsJSON, &summaryJSON, &cp.ShotHash,
		&cp.ContentHash, &cp.StructureVersion, &cp.AlgorithmVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(flagsJSON), &cp.Flags); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(summaryJSON), &cp.Summary); err != nil {
		return nil, err
	}
	return &cp, nil
}

func (s *Store) LoadValidCheckpoint(sessionID, expectedHash, expectedStructure, expectedAlgorithm string) (*Checkpoint, error) {
	cp, err := s.Latest(sessionID)
	if err != nil || cp == nil {
		return cp, err
	}
	if cp.ContentHash != expectedHash {
		_ = s.InvalidateSession(sessionID)
		return nil, &model.Error{Code: model.CodeHash, Path: "checkpoint.content_hash",
			Expected: expectedHash, Actual: cp.ContentHash,
			Message: "checkpoint content hash mismatch; all snapshots for session were discarded"}
	}
	if cp.StructureVersion != expectedStructure || cp.AlgorithmVersion != expectedAlgorithm {
		_ = s.InvalidateSession(sessionID)
		return nil, &model.Error{Code: model.CodeVersion, Path: "checkpoint.versions",
			Expected: expectedStructure + "/" + expectedAlgorithm,
			Actual:   cp.StructureVersion + "/" + cp.AlgorithmVersion,
			Message:  "checkpoint version mismatch; all snapshots for session were discarded"}
	}
	return cp, nil
}

func (s *Store) InvalidateSession(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM checkpoints WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveSession(record SessionRecord, result *model.RunResult) error {
	resultJSON := ""
	if result != nil {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		resultJSON = string(data)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
INSERT INTO sessions(id, project_id, content_hash, structure_version, algorithm_version, status, result_json, revision)
VALUES(?, ?, ?, ?, ?, ?, ?, 1)
ON CONFLICT(id) DO UPDATE SET
  project_id=excluded.project_id, content_hash=excluded.content_hash,
  structure_version=excluded.structure_version, algorithm_version=excluded.algorithm_version,
  status=excluded.status, result_json=excluded.result_json, revision=sessions.revision+1`,
		record.ID, record.ProjectID, record.ContentHash, record.StructureVersion, record.AlgorithmVersion,
		record.Status, resultJSON)
	return err
}

func (s *Store) CommitReplay(record SessionRecord, result *model.RunResult, changedShots []model.PresentedShot) error {
	resultJSON := ""
	if result != nil {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		resultJSON = string(data)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
INSERT INTO sessions(id, project_id, content_hash, structure_version, algorithm_version, status, result_json, revision)
VALUES(?, ?, ?, ?, ?, ?, ?, 1)
ON CONFLICT(id) DO UPDATE SET
  project_id=excluded.project_id, content_hash=excluded.content_hash,
  structure_version=excluded.structure_version, algorithm_version=excluded.algorithm_version,
  status=excluded.status, result_json=excluded.result_json, revision=sessions.revision+1`,
		record.ID, record.ProjectID, record.ContentHash, record.StructureVersion, record.AlgorithmVersion,
		record.Status, resultJSON); err != nil {
		return err
	}
	for _, shot := range changedShots {
		payloadJSON, err := json.Marshal(shot)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
INSERT INTO shot_cache(cache_key, project_id, shot_global_index, node_id, algorithm_version, payload_json)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(cache_key) DO UPDATE SET
  project_id=excluded.project_id, shot_global_index=excluded.shot_global_index,
  node_id=excluded.node_id, algorithm_version=excluded.algorithm_version,
  payload_json=excluded.payload_json`,
			shot.ContentHash, record.ProjectID, shot.GlobalIndex, shot.NodeID,
			model.AlgorithmVersion, string(payloadJSON)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadSession(sessionID string) (*SessionRecord, *model.RunResult, error) {
	var record SessionRecord
	var resultJSON string
	err := s.db.QueryRow(`SELECT id, project_id, content_hash, structure_version, algorithm_version, status, result_json, revision
FROM sessions WHERE id = ?`, sessionID).Scan(&record.ID, &record.ProjectID, &record.ContentHash,
		&record.StructureVersion, &record.AlgorithmVersion, &record.Status, &resultJSON, &record.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if resultJSON == "" {
		return &record, nil, nil
	}
	var result model.RunResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return &record, nil, err
	}
	return &record, &result, nil
}

func (s *Store) ListSessions(limit int) ([]SessionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, project_id, content_hash, structure_version, algorithm_version, status, '', revision
FROM sessions ORDER BY revision DESC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []SessionRecord{}
	for rows.Next() {
		var record SessionRecord
		if err := rows.Scan(&record.ID, &record.ProjectID, &record.ContentHash, &record.StructureVersion,
			&record.AlgorithmVersion, &record.Status, &record.ResultJSON, &record.Revision); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (s *Store) ListCheckpoints(sessionID string) ([]Checkpoint, error) {
	rows, err := s.db.Query(`
SELECT session_id, shot_global_index, node_id, shot_local_index, consumed_choice_offset, cursor_ms,
       flags_json, node_summary_json, shot_hash, content_hash, structure_version, algorithm_version
FROM checkpoints WHERE session_id = ? ORDER BY shot_global_index ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Checkpoint{}
	for rows.Next() {
		var cp Checkpoint
		var flagsJSON, summaryJSON string
		if err := rows.Scan(&cp.SessionID, &cp.ShotGlobalIndex, &cp.NodeID, &cp.ShotLocalIndex,
			&cp.ConsumedChoiceOffset, &cp.CursorMS, &flagsJSON, &summaryJSON, &cp.ShotHash,
			&cp.ContentHash, &cp.StructureVersion, &cp.AlgorithmVersion); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(flagsJSON), &cp.Flags); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(summaryJSON), &cp.Summary); err != nil {
			return nil, err
		}
		result = append(result, cp)
	}
	return result, rows.Err()
}

func (s *Store) WriteSequence(sessionID string) ([]int64, error) {
	rows, err := s.db.Query(`SELECT write_seq FROM write_log WHERE session_id = ? ORDER BY write_seq ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []int64{}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, err
		}
		result = append(result, seq)
	}
	return result, rows.Err()
}
