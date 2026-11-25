package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "modernc.org/sqlite"

	"replaybench/internal/eerr"
	"replaybench/internal/model"
)

type Identity struct {
	ContentHash         string
	SourceFormatVersion string
	ParamsVersion       string
	AlgorithmVersion    string
}

func IdentityFromReplay(r *model.Replay) Identity {
	return Identity{
		ContentHash: r.ContentHash, SourceFormatVersion: r.SourceFormatVersion,
		ParamsVersion: model.ParamsVersion, AlgorithmVersion: model.AlgorithmVersion,
	}
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
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

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS checkpoint_window (
  window_index INTEGER PRIMARY KEY,
  first_round INTEGER NOT NULL,
  last_round INTEGER NOT NULL,
  state_digest TEXT NOT NULL,
  consumed_offset INTEGER NOT NULL,
  next_offset INTEGER NOT NULL DEFAULT 0,
  content_hash TEXT NOT NULL,
  source_format_version TEXT NOT NULL,
  params_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  prev_digest TEXT NOT NULL DEFAULT '',
  window_digest TEXT NOT NULL UNIQUE,
  snapshot_json TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS unit_digest (
  window_index INTEGER NOT NULL,
  unit_id TEXT NOT NULL,
  digest TEXT NOT NULL,
  PRIMARY KEY(window_index, unit_id),
  FOREIGN KEY(window_index) REFERENCES checkpoint_window(window_index) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS node_cache (
  cache_key TEXT PRIMARY KEY,
  node_id TEXT NOT NULL,
  round INTEGER NOT NULL,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_node_cache_node ON node_cache(node_id, round);
`)
	return err
}

func (s *Store) CommitWindow(ctx context.Context, w *model.Window) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var maxIndex int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(window_index), 0) FROM checkpoint_window`).Scan(&maxIndex); err != nil {
		return err
	}
	if w.Index == 0 {
		w.Index = maxIndex + 1
	}
	if w.Index != maxIndex+1 {
		return eerr.New(eerr.ECheckpoint, "$.window_index", "non-serial window: expected %d got %d", maxIndex+1, w.Index)
	}
	var prevDigest string
	if maxIndex > 0 {
		if err := tx.QueryRowContext(ctx, `SELECT window_digest FROM checkpoint_window WHERE window_index=?`, maxIndex).Scan(&prevDigest); err != nil {
			return err
		}
	}
	w.PrevDigest = prevDigest
	w.WindowDigest = WindowDigest(*w)
	payload, err := json.Marshal(w.Snapshots)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_window
(window_index,first_round,last_round,state_digest,consumed_offset,next_offset,content_hash,source_format_version,params_version,algorithm_version,prev_digest,window_digest,snapshot_json)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.Index, w.FirstRound, w.LastRound, w.StateDigest, w.ConsumedOffset, w.NextOffset,
		w.ContentHash, w.SourceFormatVer, w.ParamsVersion, w.AlgorithmVersion, w.PrevDigest, w.WindowDigest, string(payload)); err != nil {
		return err
	}
	last := w.Snapshots[len(w.Snapshots)-1]
	for _, st := range last.States {
		digest := model.SHA256Hex(model.CanonicalJSON(st))
		if _, err := tx.ExecContext(ctx, `INSERT INTO unit_digest(window_index,unit_id,digest) VALUES(?,?,?)`,
			w.Index, st.UnitID, digest); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Recover(expected Identity) (*model.Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT window_index,first_round,last_round,state_digest,consumed_offset,next_offset,content_hash,
source_format_version,params_version,algorithm_version,prev_digest,window_digest,snapshot_json
FROM checkpoint_window ORDER BY window_index ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		w       model.Window
		payload string
	}
	rowsList := []row{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.w.Index, &r.w.FirstRound, &r.w.LastRound, &r.w.StateDigest, &r.w.ConsumedOffset, &r.w.NextOffset,
			&r.w.ContentHash, &r.w.SourceFormatVer, &r.w.ParamsVersion, &r.w.AlgorithmVersion,
			&r.w.PrevDigest, &r.w.WindowDigest, &r.payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(r.payload), &r.w.Snapshots); err != nil {
			r.w.WindowDigest = "__corrupt__"
		}
		rowsList = append(rowsList, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	validUntil := 0
	badIndex := -1
	var lastValid *model.Window
	prevDigest := ""
	for i, r := range rowsList {
		expectedIndex := i + 1
		w := r.w
		valid := w.Index == expectedIndex &&
			w.PrevDigest == prevDigest &&
			w.ContentHash == expected.ContentHash &&
			w.SourceFormatVer == expected.SourceFormatVersion &&
			w.ParamsVersion == expected.ParamsVersion &&
			w.AlgorithmVersion == expected.AlgorithmVersion &&
			len(w.Snapshots) > 0 && WindowDigest(w) == w.WindowDigest
		if !valid {
			badIndex = w.Index
			break
		}
		validUntil = w.Index
		prevDigest = w.WindowDigest
		ww := w
		lastValid = &ww
	}
	if badIndex >= 0 {
		if _, err := s.db.Exec(`DELETE FROM checkpoint_window WHERE window_index >= ?`, badIndex); err != nil {
			return nil, err
		}
	}
	_ = validUntil
	return lastValid, nil
}

func (s *Store) ListWindowIndexes() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT window_index FROM checkpoint_window ORDER BY window_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, err
		}
		out = append(out, idx)
	}
	return out, rows.Err()
}

func (s *Store) LoadWindows() ([]model.Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT window_index,first_round,last_round,state_digest,consumed_offset,next_offset,content_hash,
source_format_version,params_version,algorithm_version,prev_digest,window_digest,snapshot_json
FROM checkpoint_window ORDER BY window_index ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Window{}
	for rows.Next() {
		var w model.Window
		var payload string
		if err := rows.Scan(&w.Index, &w.FirstRound, &w.LastRound, &w.StateDigest, &w.ConsumedOffset, &w.NextOffset,
			&w.ContentHash, &w.SourceFormatVer, &w.ParamsVersion, &w.AlgorithmVersion,
			&w.PrevDigest, &w.WindowDigest, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &w.Snapshots); err != nil {
			return nil, fmt.Errorf("corrupt window %d: %w", w.Index, err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) GetNode(cacheKey string) ([]byte, bool, error) {
	var payload string
	err := s.db.QueryRow(`SELECT payload_json FROM node_cache WHERE cache_key=?`, cacheKey).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(payload), true, nil
}

func (s *Store) ClearNodeCache(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM node_cache`)
	return err
}

func (s *Store) PutNode(nodeID string, round int, oldKey, newKey string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO node_cache(cache_key,node_id,round,payload_json) VALUES(?,?,?,?)
ON CONFLICT(cache_key) DO UPDATE SET payload_json=excluded.payload_json, node_id=excluded.node_id, round=excluded.round`,
		newKey, nodeID, round, string(payload)); err != nil {
		return err
	}
	if oldKey != "" && oldKey != newKey {
		if _, err := tx.Exec(`DELETE FROM node_cache WHERE node_id=? AND cache_key<>?`, nodeID, newKey); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`DELETE FROM node_cache WHERE node_id=? AND cache_key<>?`, nodeID, newKey); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func WindowDigest(w model.Window) string {
	cp := w
	cp.WindowDigest = ""
	return model.SHA256Hex(model.CanonicalJSON(cp))
}

func SortedUnitDigests(snap model.RoundSnapshot) []string {
	out := []string{}
	for _, st := range snap.States {
		out = append(out, st.UnitID+":"+model.SHA256Hex(model.CanonicalJSON(st)))
	}
	sort.Strings(out)
	return out
}

func MustTempDB() (string, func(), error) {
	dir, err := os.MkdirTemp("", "replaybench-*")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "checkpoint.db")
	return path, func() { os.RemoveAll(dir) }, nil
}

var _ = fmt.Sprintf
