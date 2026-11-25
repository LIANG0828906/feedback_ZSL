package store

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
	db *sql.DB
	mu sync.Mutex
}

type SnapshotInfo struct {
	SnapshotID  string
	Generation  int
	IdentityKey string
	Status      string
	Voided      bool
	VoidReason  string
}

type IntervalRecord struct {
	IntervalNo      int
	CacheKey        string
	StartFrame      int
	EndFrame        int
	Payload         []byte
	ContentHash     string
	TimebaseVersion string
	ParamsVersion   string
	AlgoVersion     string
	Status          string
	Attempts        int
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS snapshots (
  snapshot_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  identity_key TEXT NOT NULL,
  status TEXT NOT NULL,
  void_reason TEXT NOT NULL DEFAULT '',
  algo_version TEXT NOT NULL,
  timebase_version TEXT NOT NULL,
  params_version TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(snapshot_id, generation)
);
CREATE TABLE IF NOT EXISTS interval_checkpoints (
  snapshot_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  interval_no INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  start_frame INTEGER NOT NULL,
  end_frame INTEGER NOT NULL,
  payload_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  timebase_version TEXT NOT NULL,
  params_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  status TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(snapshot_id, generation, interval_no)
);
CREATE TABLE IF NOT EXISTS interval_cache (
  cache_key TEXT PRIMARY KEY,
  payload_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  timebase_version TEXT NOT NULL,
  params_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS commit_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  snapshot_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  interval_no INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  committed_at INTEGER NOT NULL
);
`)
	return err
}

func (s *Store) BeginSnapshot(snapshotID, identityKey, algoVersion, timebaseVersion, paramsVersion string) (*SnapshotInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var gen int
	var identity, status, voidReason, oldAlgo, oldTimebase, oldParams string
	row := s.db.QueryRow(`SELECT generation, identity_key, status, void_reason, algo_version, timebase_version, params_version FROM snapshots WHERE snapshot_id=? ORDER BY generation DESC LIMIT 1`, snapshotID)
	err := row.Scan(&gen, &identity, &status, &voidReason, &oldAlgo, &oldTimebase, &oldParams)
	if err == sql.ErrNoRows {
		gen = 1
		now := time.Now().UnixNano()
		_, err = s.db.Exec(`INSERT INTO snapshots(snapshot_id,generation,identity_key,status,algo_version,timebase_version,params_version,created_at) VALUES(?,?, 'active', ?,?,?,?,?)`,
			snapshotID, gen, identityKey, algoVersion, timebaseVersion, paramsVersion, now)
		if err != nil {
			return nil, err
		}
		return &SnapshotInfo{SnapshotID: snapshotID, Generation: gen, IdentityKey: identityKey, Status: "active"}, nil
	}
	if err != nil {
		return nil, err
	}

	versionMatch := oldAlgo == algoVersion && oldTimebase == timebaseVersion && oldParams == paramsVersion
	if identity == identityKey && versionMatch && status == "active" {
		return &SnapshotInfo{SnapshotID: snapshotID, Generation: gen, IdentityKey: identity, Status: status}, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	reason := voidReason
	if identity != identityKey {
		reason = fmt.Sprintf("identity/version mismatch: old=%s new=%s", identity, identityKey)
	} else if !versionMatch {
		reason = fmt.Sprintf("version mismatch: algo=%s/%s timebase=%s/%s params=%s/%s", oldAlgo, algoVersion, oldTimebase, timebaseVersion, oldParams, paramsVersion)
	} else {
		reason = "snapshot status=" + status
	}
	if _, err = tx.Exec(`UPDATE snapshots SET status='void', void_reason=? WHERE snapshot_id=? AND generation=?`, reason, snapshotID, gen); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	newGen := gen + 1
	now := time.Now().UnixNano()
	if _, err = tx.Exec(`INSERT INTO snapshots(snapshot_id,generation,identity_key,status,void_reason,algo_version,timebase_version,params_version,created_at) VALUES(?,?,?,'active','',?,?,?,?)`,
		snapshotID, newGen, identityKey, algoVersion, timebaseVersion, paramsVersion, now); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &SnapshotInfo{SnapshotID: snapshotID, Generation: newGen, IdentityKey: identityKey, Status: "active", Voided: true, VoidReason: reason}, nil
}

func (s *Store) LoadInterval(snapshotID string, generation, intervalNo int) (*IntervalRecord, error) {
	rec := &IntervalRecord{}
	var payload string
	err := s.db.QueryRow(`SELECT cache_key,start_frame,end_frame,payload_json,content_hash,timebase_version,params_version,algo_version,status,attempts
FROM interval_checkpoints WHERE snapshot_id=? AND generation=? AND interval_no=?`,
		snapshotID, generation, intervalNo).Scan(&rec.CacheKey, &rec.StartFrame, &rec.EndFrame, &payload, &rec.ContentHash, &rec.TimebaseVersion, &rec.ParamsVersion, &rec.AlgoVersion, &rec.Status, &rec.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.Payload = []byte(payload)
	return rec, nil
}

func (s *Store) LoadCached(cacheKey string) (*IntervalRecord, error) {
	rec := &IntervalRecord{CacheKey: cacheKey}
	var payload string
	err := s.db.QueryRow(`SELECT payload_json,content_hash,timebase_version,params_version,algo_version FROM interval_cache WHERE cache_key=?`, cacheKey).
		Scan(&payload, &rec.ContentHash, &rec.TimebaseVersion, &rec.ParamsVersion, &rec.AlgoVersion)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.Payload = []byte(payload)
	return rec, nil
}

func (s *Store) DeleteCached(cacheKey string) error {
	_, err := s.db.Exec(`DELETE FROM interval_cache WHERE cache_key=?`, cacheKey)
	return err
}

func (s *Store) CommitInterval(snapshotID string, generation int, rec IntervalRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	now := time.Now().UnixNano()
	payload := string(rec.Payload)
	_, err = tx.Exec(`INSERT INTO interval_checkpoints
(snapshot_id,generation,interval_no,cache_key,start_frame,end_frame,payload_json,content_hash,timebase_version,params_version,algo_version,status,attempts,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(snapshot_id,generation,interval_no) DO UPDATE SET
 cache_key=excluded.cache_key,start_frame=excluded.start_frame,end_frame=excluded.end_frame,payload_json=excluded.payload_json,
 content_hash=excluded.content_hash,timebase_version=excluded.timebase_version,params_version=excluded.params_version,
 algo_version=excluded.algo_version,status=excluded.status,attempts=excluded.attempts,updated_at=excluded.updated_at`,
		snapshotID, generation, rec.IntervalNo, rec.CacheKey, rec.StartFrame, rec.EndFrame, payload, rec.ContentHash, rec.TimebaseVersion, rec.ParamsVersion, rec.AlgoVersion, rec.Status, rec.Attempts, now)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO interval_cache
(cache_key,payload_json,content_hash,timebase_version,params_version,algo_version,updated_at)
VALUES(?,?,?,?,?,?,?)`, rec.CacheKey, payload, rec.ContentHash, rec.TimebaseVersion, rec.ParamsVersion, rec.AlgoVersion, now)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	_, err = tx.Exec(`INSERT INTO commit_log(snapshot_id,generation,interval_no,cache_key,committed_at) VALUES(?,?,?,?,?)`,
		snapshotID, generation, rec.IntervalNo, rec.CacheKey, now)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) CommittedIntervalNumbers(snapshotID string, generation int) ([]int, error) {
	rows, err := s.db.Query(`SELECT interval_no FROM interval_checkpoints WHERE snapshot_id=? AND generation=? ORDER BY interval_no`, snapshotID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) WriteOrder(snapshotID string, generation int) ([]int, error) {
	rows, err := s.db.Query(`SELECT interval_no FROM commit_log WHERE snapshot_id=? AND generation=? ORDER BY seq`, snapshotID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) MarshalPayload(v any) ([]byte, error) { return json.Marshal(v) }

type QueueWaitRecord struct {
	IntervalNo int    `json:"interval_no"`
	Reason     string `json:"reason"`
	WaitedMS   int64  `json:"waited_ms"`
}
