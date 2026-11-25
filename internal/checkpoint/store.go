package checkpoint

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"terrainpipeline/internal/model"
)

type BlockRecord struct {
	Generation   string
	BlockID      string
	ContentHash  string
	ParamVersion string
	AlgoVersion  string
	NormalDigest string
	SlopeDigest  string
	WeightDigest string
	ResultHash   string
	SeamDigest   string
	SeamJSON     string
	Dependencies []string
}

type CommitEntry struct {
	Seq          int64  `json:"seq"`
	Generation   string `json:"generation"`
	BlockID      string `json:"block_id"`
	ContentHash  string `json:"content_hash"`
	ParamVersion string `json:"param_version"`
	AlgoVersion  string `json:"algo_version"`
	ResultHash   string `json:"result_hash"`
}

type Store struct {
	path string
	db   *sql.DB
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{path: path, db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS snapshots (
  generation TEXT NOT NULL,
  block_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  normal_digest TEXT NOT NULL,
  slope_digest TEXT NOT NULL,
  weight_digest TEXT NOT NULL,
  result_hash TEXT NOT NULL,
  seam_digest TEXT NOT NULL,
  seam_json TEXT NOT NULL,
  deps_json TEXT NOT NULL,
  PRIMARY KEY(generation, block_id)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS seam_snapshots (
  generation TEXT NOT NULL,
  seam_id TEXT NOT NULL,
  report_json TEXT NOT NULL,
  PRIMARY KEY(generation, seam_id)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS commit_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  generation TEXT NOT NULL,
  block_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  result_hash TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_commit_generation ON commit_log(generation, seq);
CREATE TABLE IF NOT EXISTS heads (
  block_id TEXT PRIMARY KEY,
  generation TEXT NOT NULL,
  seq INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  result_hash TEXT NOT NULL
);
`)
	return err
}

type Compatibility struct {
	Compatible         bool
	StoredContentHash  string
	StoredParamVersion string
	Reason             string
}

func (s *Store) CheckCompatibility(contentHash, paramVersion string) (Compatibility, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	storedHash, err := s.meta("project_content_hash")
	if err != nil {
		return Compatibility{}, err
	}
	storedParam, err := s.meta("param_version")
	if err != nil {
		return Compatibility{}, err
	}
	if storedHash == "" && storedParam == "" {
		if err := s.setMetaTx(nil, "project_content_hash", contentHash); err != nil {
			return Compatibility{}, err
		}
		if err := s.setMetaTx(nil, "param_version", paramVersion); err != nil {
			return Compatibility{}, err
		}
		if err := s.setMetaTx(nil, "algo_version", model.AlgorithmVersion); err != nil {
			return Compatibility{}, err
		}
		return Compatibility{Compatible: true, StoredContentHash: contentHash, StoredParamVersion: paramVersion}, nil
	}
	compat := Compatibility{StoredContentHash: storedHash, StoredParamVersion: storedParam}
	switch {
	case storedParam != paramVersion:
		compat.Reason = "param-version-mismatch"
	case storedHash != contentHash:
		compat.Reason = "content-hash-mismatch"
	default:
		compat.Compatible = true
	}
	if !compat.Compatible {
		if err := s.resetSnapshotsLocked(); err != nil {
			return Compatibility{}, err
		}
		if err := s.setMeta("project_content_hash", contentHash); err != nil {
			return Compatibility{}, err
		}
		if err := s.setMeta("param_version", paramVersion); err != nil {
			return Compatibility{}, err
		}
		if err := s.setMeta("algo_version", model.AlgorithmVersion); err != nil {
			return Compatibility{}, err
		}
	}
	return compat, nil
}

func (s *Store) resetSnapshotsLocked() error {
	_, err := s.db.Exec(`DELETE FROM heads; DELETE FROM snapshots; DELETE FROM seam_snapshots;`)
	return err
}

func (s *Store) meta(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (s *Store) setMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) setMetaTx(tx *sql.Tx, key, value string) error {
	if tx == nil {
		return s.setMeta(key, value)
	}
	_, err := tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) IsCommitted(generation, blockID, contentHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hash string
	err := s.db.QueryRow(`SELECT content_hash FROM snapshots WHERE generation=? AND block_id=?`, generation, blockID).Scan(&hash)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hash == contentHash, nil
}

func (s *Store) CommittedHashes(generation string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT block_id, content_hash FROM snapshots WHERE generation=?`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, err
		}
		out[id] = hash
	}
	return out, rows.Err()
}

func (s *Store) CommitBlock(rec BlockRecord) (CommitEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	depsJSON, err := json.Marshal(rec.Dependencies)
	if err != nil {
		return CommitEntry{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return CommitEntry{}, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
INSERT INTO snapshots(generation,block_id,content_hash,param_version,algo_version,normal_digest,slope_digest,weight_digest,result_hash,seam_digest,seam_json,deps_json)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(generation,block_id) DO UPDATE SET
 content_hash=excluded.content_hash,param_version=excluded.param_version,algo_version=excluded.algo_version,
 normal_digest=excluded.normal_digest,slope_digest=excluded.slope_digest,weight_digest=excluded.weight_digest,
 result_hash=excluded.result_hash,seam_digest=excluded.seam_digest,seam_json=excluded.seam_json,deps_json=excluded.deps_json`,
		rec.Generation, rec.BlockID, rec.ContentHash, rec.ParamVersion, rec.AlgoVersion,
		rec.NormalDigest, rec.SlopeDigest, rec.WeightDigest, rec.ResultHash,
		rec.SeamDigest, rec.SeamJSON, string(depsJSON)); err != nil {
		return CommitEntry{}, err
	}
	res, err := tx.Exec(`INSERT INTO commit_log(generation,block_id,content_hash,param_version,algo_version,result_hash) VALUES(?,?,?,?,?,?)`,
		rec.Generation, rec.BlockID, rec.ContentHash, rec.ParamVersion, rec.AlgoVersion, rec.ResultHash)
	if err != nil {
		return CommitEntry{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return CommitEntry{}, err
	}
	if _, err := tx.Exec(`
INSERT INTO heads(block_id,generation,seq,content_hash,param_version,algo_version,result_hash)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(block_id) DO UPDATE SET generation=excluded.generation,seq=excluded.seq,content_hash=excluded.content_hash,
 param_version=excluded.param_version,algo_version=excluded.algo_version,result_hash=excluded.result_hash`,
		rec.BlockID, rec.Generation, seq, rec.ContentHash, rec.ParamVersion, rec.AlgoVersion, rec.ResultHash); err != nil {
		return CommitEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return CommitEntry{}, err
	}
	return CommitEntry{Seq: seq, Generation: rec.Generation, BlockID: rec.BlockID, ContentHash: rec.ContentHash, ParamVersion: rec.ParamVersion, AlgoVersion: rec.AlgoVersion, ResultHash: rec.ResultHash}, nil
}

func (s *Store) PutSeamReport(generation, seamID, reportJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO seam_snapshots(generation,seam_id,report_json) VALUES(?,?,?)
ON CONFLICT(generation,seam_id) DO UPDATE SET report_json=excluded.report_json`, generation, seamID, reportJSON)
	return err
}

func (s *Store) CommitSequence(generation string) ([]CommitEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT seq,generation,block_id,content_hash,param_version,algo_version,result_hash FROM commit_log WHERE generation=? ORDER BY seq ASC`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitEntry
	for rows.Next() {
		var e CommitEntry
		if err := rows.Scan(&e.Seq, &e.Generation, &e.BlockID, &e.ContentHash, &e.ParamVersion, &e.AlgoVersion, &e.ResultHash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) HeadSequence() ([]CommitEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT seq,generation,block_id,content_hash,param_version,algo_version,result_hash FROM heads ORDER BY block_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitEntry
	for rows.Next() {
		var e CommitEntry
		if err := rows.Scan(&e.Seq, &e.Generation, &e.BlockID, &e.ContentHash, &e.ParamVersion, &e.AlgoVersion, &e.ResultHash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) LoadHeadResultHashes() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT block_id, result_hash FROM heads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, err
		}
		out[id] = hash
	}
	return out, rows.Err()
}

func (s *Store) Path() string { return s.path }

func SQLiteDSN(path string) string {
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
}
