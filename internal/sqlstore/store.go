package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	acoustic "levelacoustic"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, checkpointError("open", "database path is empty", nil)
	}
	if path != ":memory:" && !containsMemory(path) {
		if directory := filepath.Dir(path); directory != "." {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return nil, checkpointError("open", err.Error(), err)
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, checkpointError("open", err.Error(), err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) initialize(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		return checkpointError("initialize", err.Error(), err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return checkpointError("initialize", err.Error(), err)
	}
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS checkpoint_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS room_checkpoints (
  room_id INTEGER PRIMARY KEY,
  sequence INTEGER NOT NULL UNIQUE,
  content_hash TEXT NOT NULL,
  params_version INTEGER NOT NULL,
  algorithm_version TEXT NOT NULL,
  payload TEXT NOT NULL
);`); err != nil {
		return checkpointError("initialize", err.Error(), err)
	}
	return nil
}

func (s *Store) Load(ctx context.Context, projectFingerprint string, paramsVersion int64, algorithmVersion string) (acoustic.CheckpointLoadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return acoustic.CheckpointLoadResult{}, checkpointError("load", "database is closed", nil)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return acoustic.CheckpointLoadResult{}, checkpointError("load", err.Error(), err)
	}
	defer tx.Rollback()

	meta, err := readMeta(ctx, tx)
	if err != nil {
		return acoustic.CheckpointLoadResult{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT room_id, sequence, content_hash, params_version, algorithm_version, payload FROM room_checkpoints ORDER BY sequence`)
	if err != nil {
		return acoustic.CheckpointLoadResult{}, checkpointError("load", err.Error(), err)
	}
	defer rows.Close()

	var result acoustic.CheckpointLoadResult
	expectedSequence := 1
	lastRoomID := 0
	for rows.Next() {
		var roomID, sequence int
		var contentHash, algorithm string
		var params int64
		var payload string
		if err := rows.Scan(&roomID, &sequence, &contentHash, &params, &algorithm, &payload); err != nil {
			return acoustic.CheckpointLoadResult{}, checkpointError("load", err.Error(), err)
		}
		if sequence != expectedSequence {
			result.Invalidated = true
			result.InvalidReason = fmt.Sprintf("checkpoint sequence gap at room %d", roomID)
			break
		}
		if roomID <= lastRoomID {
			result.Invalidated = true
			result.InvalidReason = fmt.Sprintf("checkpoint room order is not strictly increasing at room %d", roomID)
			break
		}
		if params != paramsVersion || algorithm != algorithmVersion {
			result.Invalidated = true
			result.InvalidReason = fmt.Sprintf("checkpoint version mismatch at room %d", roomID)
			break
		}
		var snapshot acoustic.RoomSnapshot
		if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
			result.Invalidated = true
			result.InvalidReason = fmt.Sprintf("checkpoint payload is invalid at room %d", roomID)
			break
		}
		digest, err := acoustic.SnapshotDigest(snapshot)
		if err != nil {
			return acoustic.CheckpointLoadResult{}, err
		}
		if digest != contentHash || snapshot.ContentHash != contentHash {
			result.Invalidated = true
			result.InvalidReason = fmt.Sprintf("checkpoint content hash mismatch at room %d", roomID)
			break
		}
		if snapshot.RoomID != roomID || snapshot.CommitSequence != sequence ||
			snapshot.ParamsVersion != paramsVersion || snapshot.AlgorithmVersion != algorithmVersion ||
			snapshot.ProjectFingerprint != projectFingerprint {
			result.Invalidated = true
			result.InvalidReason = fmt.Sprintf("checkpoint metadata mismatch at room %d", roomID)
			break
		}
		result.Snapshots = append(result.Snapshots, snapshot)
		expectedSequence++
		lastRoomID = roomID
	}
	if err := rows.Err(); err != nil {
		return acoustic.CheckpointLoadResult{}, checkpointError("load", err.Error(), err)
	}
	if !result.Invalidated {
		if len(result.Snapshots) == 0 {
			if len(meta) > 0 {
				result.Invalidated = true
				result.InvalidReason = "checkpoint metadata exists without room rows"
			}
		} else {
			if meta["project_fingerprint"] != projectFingerprint ||
				meta["params_version"] != strconv.FormatInt(paramsVersion, 10) ||
				meta["algorithm_version"] != algorithmVersion {
				result.Invalidated = true
				result.InvalidReason = "checkpoint set metadata mismatch"
			}
		}
	}
	if result.Invalidated {
		rows.Close()
		if err := discardTx(ctx, tx); err != nil {
			return acoustic.CheckpointLoadResult{}, err
		}
		result.Snapshots = nil
	}
	if err := tx.Commit(); err != nil {
		return acoustic.CheckpointLoadResult{}, checkpointError("load", err.Error(), err)
	}
	return result, nil
}

func (s *Store) Commit(ctx context.Context, snapshot acoustic.RoomSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return checkpointError("commit", "database is closed", nil)
	}
	digest, err := acoustic.SnapshotDigest(snapshot)
	if err != nil {
		return err
	}
	if snapshot.ContentHash != digest {
		return checkpointError("commit", "snapshot content hash is not self-consistent", nil)
	}
	if snapshot.RoomID <= 0 || snapshot.CommitSequence <= 0 ||
		snapshot.ParamsVersion <= 0 || snapshot.AlgorithmVersion == "" || snapshot.ProjectFingerprint == "" {
		return checkpointError("commit", "snapshot metadata is incomplete", nil)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return checkpointError("commit", err.Error(), err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return checkpointError("commit", err.Error(), err)
	}
	defer tx.Rollback()
	meta, err := readMeta(ctx, tx)
	if err != nil {
		return err
	}
	var rowCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM room_checkpoints`).Scan(&rowCount); err != nil {
		return checkpointError("commit", err.Error(), err)
	}
	if rowCount == 0 {
		if len(meta) != 0 {
			return checkpointError("commit", "empty checkpoint set has metadata", nil)
		}
		if snapshot.CommitSequence != 1 {
			return checkpointError("commit", "first checkpoint sequence must be 1", nil)
		}
		if err := writeMeta(ctx, tx, snapshot); err != nil {
			return err
		}
	} else {
		if meta["project_fingerprint"] != snapshot.ProjectFingerprint ||
			meta["params_version"] != strconv.FormatInt(snapshot.ParamsVersion, 10) ||
			meta["algorithm_version"] != snapshot.AlgorithmVersion {
			return checkpointError("commit", "checkpoint set metadata mismatch", nil)
		}
		var maxSequence, lastRoomID int
		if err := tx.QueryRowContext(ctx, `SELECT MAX(sequence), MAX(room_id) FROM room_checkpoints`).Scan(&maxSequence, &lastRoomID); err != nil {
			return checkpointError("commit", err.Error(), err)
		}
		if snapshot.CommitSequence != maxSequence+1 {
			return checkpointError("commit", fmt.Sprintf("checkpoint sequence must be %d", maxSequence+1), nil)
		}
		if snapshot.RoomID <= lastRoomID {
			return checkpointError("commit", "room commit order must be strictly increasing", nil)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO room_checkpoints(room_id, sequence, content_hash, params_version, algorithm_version, payload)
VALUES(?, ?, ?, ?, ?, ?)`, snapshot.RoomID, snapshot.CommitSequence, snapshot.ContentHash, snapshot.ParamsVersion, snapshot.AlgorithmVersion, string(payload)); err != nil {
		return checkpointError("commit", err.Error(), err)
	}
	if err := tx.Commit(); err != nil {
		return checkpointError("commit", err.Error(), err)
	}
	return nil
}

func (s *Store) Discard(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return checkpointError("discard", "database is closed", nil)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return checkpointError("discard", err.Error(), err)
	}
	defer tx.Rollback()
	if err := discardTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return checkpointError("discard", err.Error(), err)
	}
	return nil
}

func readMeta(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key, value FROM checkpoint_meta`)
	if err != nil {
		return nil, checkpointError("meta", err.Error(), err)
	}
	defer rows.Close()
	meta := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, checkpointError("meta", err.Error(), err)
		}
		meta[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, checkpointError("meta", err.Error(), err)
	}
	return meta, nil
}

func writeMeta(ctx context.Context, tx *sql.Tx, snapshot acoustic.RoomSnapshot) error {
	values := map[string]string{
		"project_fingerprint": snapshot.ProjectFingerprint,
		"params_version":      strconv.FormatInt(snapshot.ParamsVersion, 10),
		"algorithm_version":   snapshot.AlgorithmVersion,
	}
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_meta(key, value) VALUES(?, ?)`, key, value); err != nil {
			return checkpointError("meta", err.Error(), err)
		}
	}
	return nil
}

func discardTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM room_checkpoints`); err != nil {
		return checkpointError("discard", err.Error(), err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_meta`); err != nil {
		return checkpointError("discard", err.Error(), err)
	}
	return nil
}

func checkpointError(op, message string, cause error) error {
	domain := &acoustic.Error{Code: acoustic.CodeCheckpoint, Op: op, Message: message}
	if cause != nil {
		domain.Cause = cause.Error()
		domain.CauseError = cause
		if errors.Is(cause, context.Canceled) {
			domain.Cause = context.Canceled.Error()
		}
	}
	return domain
}

func containsMemory(path string) bool {
	return path == ":memory:" || (len(path) >= 13 && path[:13] == "file::memory:")
}
