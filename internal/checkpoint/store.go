package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"semanticshading/internal/apierr"
	"semanticshading/internal/model"
)

type Identity struct {
	InputHash        string
	ManifestHash     string
	TruthFingerprint string
	ParamVersion     string
	AlgoVersion      string
}

type ResetInfo struct {
	Reset          bool     `json:"reset"`
	Reason         string   `json:"reason,omitempty"`
	MismatchedKeys []string `json:"mismatched_keys,omitempty"`
	OldIdentity    Identity `json:"old_identity,omitempty"`
	NewIdentity    Identity `json:"new_identity,omitempty"`
}

type Store struct {
	db       *sql.DB
	path     string
	identity Identity
	mu       sync.Mutex
}

func PrepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return err
	}
	name := file.Name()
	if _, err := file.WriteString("ok"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Remove(name)
}

func Open(path string, identity Identity) (*Store, *ResetInfo, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := PrepareDir(dir); err != nil {
			return nil, nil, err
		}
	}
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("configure sqlite: %w", err)
	}
	s := &Store{db: db, path: path, identity: identity}
	if err := s.createSchema(); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	reset, err := s.checkIdentity(identity)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return s, reset, nil
}

func OpenExisting(path string) (*Store, error) {
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout(5000); PRAGMA query_only=ON;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure read-only sqlite: %w", err)
	}
	s := &Store{db: db, path: path}
	meta, err := s.metaMap()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.identity = Identity{
		InputHash:        meta["input_hash"],
		ManifestHash:     meta["manifest_hash"],
		TruthFingerprint: meta["truth_fingerprint"],
		ParamVersion:     meta["param_version"],
		AlgoVersion:      meta["algo_version"],
	}
	return s, nil
}

func (s *Store) createSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS snapshot_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS block_checkpoints (
  block_index INTEGER PRIMARY KEY,
  commit_seq INTEGER NOT NULL UNIQUE,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  x0 INTEGER NOT NULL,
  y0 INTEGER NOT NULL,
  x1 INTEGER NOT NULL,
  y1 INTEGER NOT NULL,
  input_hash TEXT NOT NULL,
  manifest_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  checkpoint_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_block_checkpoints_commit ON block_checkpoints(commit_seq);
`)
	return err
}

func (s *Store) metaMap() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM snapshot_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, rows.Err()
}

func (s *Store) checkIdentity(next Identity) (*ResetInfo, error) {
	oldMap, err := s.metaMap()
	if err != nil {
		return nil, err
	}
	keys := []struct {
		key string
		old string
		new string
	}{
		{"input_hash", oldMap["input_hash"], next.InputHash},
		{"manifest_hash", oldMap["manifest_hash"], next.ManifestHash},
		{"truth_fingerprint", oldMap["truth_fingerprint"], next.TruthFingerprint},
		{"param_version", oldMap["param_version"], next.ParamVersion},
		{"algo_version", oldMap["algo_version"], next.AlgoVersion},
	}
	if len(oldMap) == 0 {
		return &ResetInfo{Reset: false}, s.writeIdentity(next)
	}
	info := &ResetInfo{Reset: false, OldIdentity: Identity{
		InputHash: oldMap["input_hash"], ManifestHash: oldMap["manifest_hash"], TruthFingerprint: oldMap["truth_fingerprint"],
		ParamVersion: oldMap["param_version"], AlgoVersion: oldMap["algo_version"],
	}, NewIdentity: next}
	for _, item := range keys {
		if item.old != item.new {
			info.MismatchedKeys = append(info.MismatchedKeys, item.key)
		}
	}
	if len(info.MismatchedKeys) == 0 {
		return info, nil
	}
	info.Reset = true
	info.Reason = "input hash or parameter/algorithm version mismatch; snapshot invalidated"
	if err := s.resetLocked(next); err != nil {
		return nil, err
	}
	return info, nil
}

func (s *Store) writeIdentity(id Identity) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range [][2]string{
		{"input_hash", id.InputHash},
		{"manifest_hash", id.ManifestHash},
		{"truth_fingerprint", id.TruthFingerprint},
		{"param_version", id.ParamVersion},
		{"algo_version", id.AlgoVersion},
	} {
		if _, err := tx.Exec(`INSERT INTO snapshot_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, item[0], item[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) resetLocked(id Identity) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DROP TABLE IF EXISTS block_checkpoints`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM snapshot_meta`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := s.createSchema(); err != nil {
		return err
	}
	return s.writeIdentity(id)
}

func (s *Store) Reset(id Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identity = id
	return s.resetLocked(id)
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) Identity() Identity { return s.identity }

func (s *Store) Commit(ctx context.Context, cp model.Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	b := cp.Block.Block
	_, err = tx.ExecContext(ctx, `
INSERT INTO block_checkpoints(
 block_index, commit_seq, block_x, block_y, x0, y0, x1, y1,
 input_hash, manifest_hash, param_version, algo_version, checkpoint_json
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(block_index) DO UPDATE SET
 commit_seq=excluded.commit_seq, block_x=excluded.block_x, block_y=excluded.block_y,
 x0=excluded.x0, y0=excluded.y0, x1=excluded.x1, y1=excluded.y1,
 input_hash=excluded.input_hash, manifest_hash=excluded.manifest_hash,
 param_version=excluded.param_version, algo_version=excluded.algo_version,
 checkpoint_json=excluded.checkpoint_json`,
		b.Index, cp.CommitOrder, b.BX, b.BY, b.X0, b.Y0, b.X1, b.Y1,
		cp.InputHash, cp.ManifestHash, cp.ParamVersion, cp.AlgoVersion, string(payload))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Count() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM block_checkpoints`).Scan(&count)
	return count, err
}

func (s *Store) CommitSequence() ([]int, error) {
	rows, err := s.db.Query(`SELECT block_index FROM block_checkpoints ORDER BY commit_seq ASC, block_index ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []int{}
	for rows.Next() {
		var index int
		if err := rows.Scan(&index); err != nil {
			return nil, err
		}
		result = append(result, index)
	}
	return result, rows.Err()
}

func (s *Store) LoadCheckpoints() ([]model.Checkpoint, error) {
	rows, err := s.db.Query(`SELECT checkpoint_json FROM block_checkpoints ORDER BY block_index ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.Checkpoint{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var cp model.Checkpoint
		if err := json.Unmarshal([]byte(payload), &cp); err != nil {
			return nil, apierr.New(apierr.EDecode, "checkpoint_json", "cannot decode persisted checkpoint: %v", err)
		}
		result = append(result, cp)
	}
	return result, rows.Err()
}

func (s *Store) LoadBlockRecords() ([]model.BlockRecord, error) {
	checkpoints, err := s.LoadCheckpoints()
	if err != nil {
		return nil, err
	}
	records := make([]model.BlockRecord, 0, len(checkpoints))
	for _, cp := range checkpoints {
		records = append(records, cp.Block)
	}
	return records, nil
}
