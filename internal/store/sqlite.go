package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"roomreverb/internal/model"
)

var ErrSnapshotInvalid = errors.New("checkpoint snapshot invalid")

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		if err := initSchema(db); err != nil {
			_ = db.Close()
			return nil, err
		}
		return &Store{db: db}, nil
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS room_checkpoints (
  room_id TEXT PRIMARY KEY,
  revision INTEGER NOT NULL,
  commit_order INTEGER NOT NULL UNIQUE,
  payload_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);`)
	return err
}

func HashRoomResult(result model.RoomResult) string {
	result.ContentHash = ""
	data, _ := json.Marshal(result)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *Store) SaveRoom(result model.RoomResult, revision int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	hash := HashRoomResult(result)
	result.ContentHash = hash
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
INSERT INTO room_checkpoints
(room_id, revision, commit_order, payload_json, content_hash, param_version, algo_version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(room_id) DO UPDATE SET
  revision=excluded.revision,
  commit_order=excluded.commit_order,
  payload_json=excluded.payload_json,
  content_hash=excluded.content_hash,
  param_version=excluded.param_version,
  algo_version=excluded.algo_version,
  created_at=excluded.created_at`,
		result.RoomID, revision, result.CommitOrder, string(payload), hash,
		result.ParamVersion, result.AlgoVersion, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('last_commit_order',?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(result.CommitOrder)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadValid(expectedParamVersion, expectedAlgoVersion string) ([]model.RoomResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`SELECT payload_json, content_hash, param_version, algo_version FROM room_checkpoints ORDER BY commit_order ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []model.RoomResult
	invalid := false
	for rows.Next() {
		var payload, hash, paramVersion, algoVersion string
		if err := rows.Scan(&payload, &hash, &paramVersion, &algoVersion); err != nil {
			return nil, err
		}
		var result model.RoomResult
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			invalid = true
			break
		}
		if HashRoomResult(result) != hash || result.ContentHash != hash ||
			paramVersion != expectedParamVersion || algoVersion != expectedAlgoVersion ||
			result.ParamVersion != expectedParamVersion || result.AlgoVersion != expectedAlgoVersion {
			invalid = true
			break
		}
		result.ContentHash = hash
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if invalid {
		_ = rows.Close()
		_ = s.resetLocked()
		return nil, ErrSnapshotInvalid
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].CommitOrder < results[j].CommitOrder })
	return results, nil
}

func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetLocked()
}

func (s *Store) resetLocked() error {
	if _, err := s.db.Exec(`DELETE FROM room_checkpoints`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM meta`)
	return err
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}
