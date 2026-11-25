// Package checkpoint provides the SQLite-backed window checkpoint store.
package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Signature identifies one exact project/parameter/algorithm window.
type Signature struct {
	GlobalHash       string `json:"global_hash"`
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
}

// RoomCheckpoint is the atomic per-room persistence payload.
type RoomCheckpoint struct {
	GlobalHash       string          `json:"global_hash"`
	RoomID           string          `json:"room_id"`
	CommitIndex      int             `json:"commit_index"`
	ContentHash      string          `json:"content_hash"`
	ParamVersion     string          `json:"param_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
	SurfaceSnapshot  json.RawMessage `json:"surface_snapshot"`
	DoorSnapshot     json.RawMessage `json:"door_snapshot"`
	ChainSummary     json.RawMessage `json:"chain_summary"`
	ResultJSON       json.RawMessage `json:"result_json"`
	Status           string          `json:"status"`
	CommittedAt      string          `json:"committed_at"`
}

type WindowState struct {
	Signature Signature
	RoomOrder []string
	Recovered map[string]RoomCheckpoint
	Discarded bool
	Reason    string
}

// Store serializes all writes with a mutex and one connection.
type Store struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error { return s.db.Close() }

// LoadWindow reads the current meta window and all committed room rows without
// applying signature invalidation. Callers use it before an explicit edit
// migration to reconstruct the previous persistent report.
func (s *Store) LoadWindow(ctx context.Context) (Signature, []string, map[string]RoomCheckpoint, error) {
	var sig Signature
	var orderJSON string
	err := s.db.QueryRowContext(ctx, `SELECT global_hash,param_version,algorithm_version,room_order_json FROM window_meta WHERE id=1`).
		Scan(&sig.GlobalHash, &sig.ParamVersion, &sig.AlgorithmVersion, &orderJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return sig, nil, map[string]RoomCheckpoint{}, nil
	}
	if err != nil {
		return sig, nil, nil, err
	}
	var order []string
	if err := json.Unmarshal([]byte(orderJSON), &order); err != nil {
		return sig, nil, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT global_hash,room_id,commit_index,content_hash,param_version,algorithm_version,surface_snapshot,door_snapshot,chain_summary,result_json,status,committed_at FROM room_checkpoints WHERE status='committed'`)
	if err != nil {
		return sig, nil, nil, err
	}
	defer rows.Close()
	out := map[string]RoomCheckpoint{}
	for rows.Next() {
		var cp RoomCheckpoint
		var surface, doors, chains, result string
		if err := rows.Scan(&cp.GlobalHash, &cp.RoomID, &cp.CommitIndex, &cp.ContentHash, &cp.ParamVersion, &cp.AlgorithmVersion, &surface, &doors, &chains, &result, &cp.Status, &cp.CommittedAt); err != nil {
			return sig, nil, nil, err
		}
		cp.SurfaceSnapshot = json.RawMessage(surface)
		cp.DoorSnapshot = json.RawMessage(doors)
		cp.ChainSummary = json.RawMessage(chains)
		cp.ResultJSON = json.RawMessage(result)
		out[cp.RoomID] = cp
	}
	return sig, order, out, rows.Err()
}

func (s *Store) initSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS window_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  global_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  room_order_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS room_checkpoints (
  global_hash TEXT NOT NULL,
  room_id TEXT NOT NULL,
  commit_index INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  surface_snapshot TEXT NOT NULL,
  door_snapshot TEXT NOT NULL,
  chain_summary TEXT NOT NULL,
  result_json TEXT NOT NULL,
  status TEXT NOT NULL,
  committed_at TEXT NOT NULL,
  PRIMARY KEY (global_hash, room_id)
);
CREATE TABLE IF NOT EXISTS commit_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  global_hash TEXT NOT NULL,
  room_id TEXT NOT NULL,
  commit_index INTEGER NOT NULL,
  committed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_commit_log_hash ON commit_log(global_hash, seq);
`)
	return err
}

// InitializeWindow resumes only when global content hash and both versions
// match. Any mismatch atomically discards all old window snapshots.
func (s *Store) InitializeWindow(ctx context.Context, sig Signature, roomOrder []string) (*WindowState, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	orderJSON, err := json.Marshal(roomOrder)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var stored Signature
	var storedOrderJSON string
	var updatedAt string
	row := tx.QueryRowContext(ctx, `SELECT global_hash, param_version, algorithm_version, room_order_json, updated_at FROM window_meta WHERE id=1`)
	switch err := row.Scan(&stored.GlobalHash, &stored.ParamVersion, &stored.AlgorithmVersion, &storedOrderJSON, &updatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO window_meta(id,global_hash,param_version,algorithm_version,room_order_json,updated_at) VALUES(1,?,?,?,?,?)`,
			sig.GlobalHash, sig.ParamVersion, sig.AlgorithmVersion, string(orderJSON), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &WindowState{Signature: sig, RoomOrder: append([]string{}, roomOrder...), Recovered: map[string]RoomCheckpoint{}}, nil
	case err != nil:
		return nil, err
	}

	discardReason := ""
	if stored != sig {
		discardReason = fmt.Sprintf("signature mismatch: stored=%+v current=%+v", stored, sig)
	} else {
		var storedOrder []string
		if err := json.Unmarshal([]byte(storedOrderJSON), &storedOrder); err != nil || stringsJoin(storedOrder) != stringsJoin(roomOrder) {
			discardReason = "room order mismatch"
		}
	}

	state := &WindowState{Signature: sig, RoomOrder: append([]string{}, roomOrder...), Recovered: map[string]RoomCheckpoint{}}
	if discardReason != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM room_checkpoints`); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM commit_log`); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE window_meta SET global_hash=?, param_version=?, algorithm_version=?, room_order_json=?, updated_at=? WHERE id=1`,
			sig.GlobalHash, sig.ParamVersion, sig.AlgorithmVersion, string(orderJSON), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		state.Discarded = true
		state.Reason = discardReason
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return state, nil
	}

	rows, err := tx.QueryContext(ctx, `SELECT global_hash,room_id,commit_index,content_hash,param_version,algorithm_version,surface_snapshot,door_snapshot,chain_summary,result_json,status,committed_at FROM room_checkpoints WHERE global_hash=? AND status='committed'`, sig.GlobalHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cp RoomCheckpoint
		var surface, doors, chains, result string
		if err := rows.Scan(&cp.GlobalHash, &cp.RoomID, &cp.CommitIndex, &cp.ContentHash, &cp.ParamVersion, &cp.AlgorithmVersion, &surface, &doors, &chains, &result, &cp.Status, &cp.CommittedAt); err != nil {
			return nil, err
		}
		cp.SurfaceSnapshot = json.RawMessage(surface)
		cp.DoorSnapshot = json.RawMessage(doors)
		cp.ChainSummary = json.RawMessage(chains)
		cp.ResultJSON = json.RawMessage(result)
		if cp.ParamVersion != sig.ParamVersion || cp.AlgorithmVersion != sig.AlgorithmVersion {
			discardReason = "room version mismatch for " + cp.RoomID
			break
		}
		state.Recovered[cp.RoomID] = cp
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if discardReason != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM room_checkpoints`); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM commit_log`); err != nil {
			return nil, err
		}
		state.Recovered = map[string]RoomCheckpoint{}
		state.Discarded = true
		state.Reason = discardReason
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return state, nil
}

// SaveRoom atomically writes one room checkpoint and its ordered commit-log row.
func (s *Store) SaveRoom(ctx context.Context, cp RoomCheckpoint) error {
	if cp.CommittedAt == "" {
		cp.CommittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if cp.Status == "" {
		cp.Status = "committed"
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO room_checkpoints
(global_hash,room_id,commit_index,content_hash,param_version,algorithm_version,surface_snapshot,door_snapshot,chain_summary,result_json,status,committed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		cp.GlobalHash, cp.RoomID, cp.CommitIndex, cp.ContentHash, cp.ParamVersion, cp.AlgorithmVersion,
		string(cp.SurfaceSnapshot), string(cp.DoorSnapshot), string(cp.ChainSummary), string(cp.ResultJSON), cp.Status, cp.CommittedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO commit_log(global_hash,room_id,commit_index,committed_at) VALUES(?,?,?,?)`,
		cp.GlobalHash, cp.RoomID, cp.CommitIndex, cp.CommittedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitOrder(ctx context.Context, globalHash string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT room_id FROM commit_log WHERE global_hash=? ORDER BY seq ASC`, globalHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) CountCheckpoints(ctx context.Context, globalHash string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM room_checkpoints WHERE global_hash=?`, globalHash).Scan(&n)
	return n, err
}

func stringsJoin(xs []string) string {
	out := ""
	for _, x := range xs {
		out += x + "\x00"
	}
	return out
}
