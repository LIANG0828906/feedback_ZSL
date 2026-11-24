package checkpoint

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"integritykernel/internal/core"
)

type Item struct {
	Frame            int
	Type             string
	CacheKey         string
	ContentHash      string
	ParamHash        string
	AlgorithmVersion string
	Digest           string
	PayloadJSON      []byte
}

type Meta struct {
	RootHash         string
	ParamVersion     string
	AlgorithmVersion string
	ParamsJSON       string
}

type LogRecord struct {
	Ordinal     int64  `json:"ordinal"`
	BatchSeq    int    `json:"batch_seq"`
	ItemOrdinal int    `json:"item_ordinal"`
	Frame       int    `json:"frame"`
	Type        string `json:"type"`
	CacheKey    string `json:"cache_key"`
	Digest      string `json:"digest"`
	Action      string `json:"action"`
}

type StoredEvidence struct {
	Item
	BatchSeq int `json:"batch_seq"`
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if err := store.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS batches (
  seq INTEGER PRIMARY KEY,
  kind TEXT NOT NULL,
  status TEXT NOT NULL,
  first_frame INTEGER NOT NULL,
  last_frame INTEGER NOT NULL,
  item_count INTEGER NOT NULL DEFAULT 0,
  digest TEXT NOT NULL DEFAULT '',
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence (
  cache_key TEXT PRIMARY KEY,
  frame_index INTEGER NOT NULL,
  evidence_type TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_hash TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  digest TEXT NOT NULL,
  batch_seq INTEGER NOT NULL,
  committed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence_log (
  ordinal INTEGER PRIMARY KEY AUTOINCREMENT,
  batch_seq INTEGER NOT NULL,
  item_ordinal INTEGER NOT NULL,
  frame_index INTEGER NOT NULL,
  evidence_type TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  digest TEXT NOT NULL,
  action TEXT NOT NULL DEFAULT 'compute'
);
CREATE TABLE IF NOT EXISTS current_evidence (
  frame_index INTEGER NOT NULL,
  evidence_type TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  PRIMARY KEY(frame_index, evidence_type)
);
CREATE INDEX IF NOT EXISTS idx_evidence_frame_type ON evidence(frame_index, evidence_type);
CREATE INDEX IF NOT EXISTS idx_log_batch ON evidence_log(batch_seq, item_ordinal);
`)
	return err
}

func (s *Store) Reset(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`
DELETE FROM meta;
DELETE FROM batches;
DELETE FROM evidence;
DELETE FROM evidence_log;
DELETE FROM current_evidence;
DELETE FROM sqlite_sequence WHERE name='evidence_log';
`)
	if err != nil {
		return core.NewError(core.CodeTxn, "reset checkpoint failed: %v (%s)", err, reason)
	}
	return nil
}

func (s *Store) LoadMeta() (Meta, bool, error) {
	var meta Meta
	rows, err := s.db.Query(`SELECT key, value FROM meta WHERE key IN ('root_hash','param_version','algo_version')`)
	if err != nil {
		return meta, false, err
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return meta, false, err
		}
		found[k] = v
	}
	if err := rows.Err(); err != nil {
		return meta, false, err
	}
	if len(found) == 0 {
		return meta, false, nil
	}
	meta.RootHash = found["root_hash"]
	meta.ParamVersion = found["param_version"]
	meta.AlgorithmVersion = found["algo_version"]
	return meta, true, nil
}

func (s *Store) SnapshotMatches(rootHash, paramVersion, algoVersion string) (bool, string, error) {
	meta, found, err := s.LoadMeta()
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "empty_snapshot", nil
	}
	if meta.RootHash != rootHash {
		return false, "content_hash_mismatch", nil
	}
	if meta.ParamVersion != paramVersion {
		return false, "param_version_mismatch", nil
	}
	if meta.AlgorithmVersion != algoVersion {
		return false, "algorithm_version_mismatch", nil
	}
	return true, "", nil
}

func (s *Store) BeginBatch(seq int, kind string, firstFrame, lastFrame int, paramVersion, algoVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`
INSERT INTO batches(seq,kind,status,first_frame,last_frame,item_count,digest,param_version,algo_version,error,updated_at)
VALUES(?,?, 'pending', ?, ?, 0, '', ?, ?, '', ?)
ON CONFLICT(seq) DO UPDATE SET status='pending', kind=excluded.kind, first_frame=excluded.first_frame,
 last_frame=excluded.last_frame, param_version=excluded.param_version, algo_version=excluded.algo_version,
 error='', updated_at=excluded.updated_at`,
		seq, kind, firstFrame, lastFrame, paramVersion, algoVersion, now)
	return err
}

type CommitHook func(seq int) error

func (s *Store) CommitBatch(seq int, kind string, items []Item, rootHash, paramVersion, algoVersion, paramsJSON string, hook CommitHook) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if hook != nil {
		if err := hook(seq); err != nil {
			return core.WithBatch(core.NewError(core.CodeTxn, "batch %d interrupted before atomic commit: %v", seq, err), seq)
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return core.WithBatch(core.NewError(core.CodeTxn, "begin transaction batch %d: %v", seq, err), seq)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	for ordinal, item := range items {
		if len(item.PayloadJSON) == 0 {
			return core.WithBatch(core.NewError(core.CodeTxn, "batch %d item %d has empty payload", seq, ordinal), seq)
		}
		_, err = tx.Exec(`
INSERT INTO evidence(cache_key,frame_index,evidence_type,content_hash,param_hash,algo_version,payload_json,digest,batch_seq,committed_at)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(cache_key) DO UPDATE SET payload_json=excluded.payload_json, digest=excluded.digest,
 batch_seq=excluded.batch_seq, committed_at=excluded.committed_at`,
			item.CacheKey, item.Frame, item.Type, item.ContentHash, item.ParamHash, item.AlgorithmVersion,
			string(item.PayloadJSON), item.Digest, seq, now)
		if err != nil {
			return core.WithBatch(core.NewError(core.CodeTxn, "write evidence batch %d: %v", seq, err), seq)
		}
		_, err = tx.Exec(`
INSERT INTO evidence_log(batch_seq,item_ordinal,frame_index,evidence_type,cache_key,digest,action)
VALUES(?,?,?,?,?,?, 'compute')`,
			seq, ordinal, item.Frame, item.Type, item.CacheKey, item.Digest)
		if err != nil {
			return core.WithBatch(core.NewError(core.CodeTxn, "write evidence log batch %d: %v", seq, err), seq)
		}
		_, err = tx.Exec(`
INSERT INTO current_evidence(frame_index,evidence_type,cache_key)
VALUES(?,?,?)
ON CONFLICT(frame_index,evidence_type) DO UPDATE SET cache_key=excluded.cache_key`,
			item.Frame, item.Type, item.CacheKey)
		if err != nil {
			return core.WithBatch(core.NewError(core.CodeTxn, "update current evidence batch %d: %v", seq, err), seq)
		}
	}
	digest := batchDigest(items)
	firstFrame, lastFrame := 0, 0
	if len(items) > 0 {
		firstFrame, lastFrame = items[0].Frame, items[len(items)-1].Frame
	}
	_, err = tx.Exec(`
INSERT INTO batches(seq,kind,status,first_frame,last_frame,item_count,digest,param_version,algo_version,error,updated_at)
VALUES(?,?, 'committed', ?, ?, ?, ?, ?, ?, '', ?)
ON CONFLICT(seq) DO UPDATE SET status='committed', kind=excluded.kind, first_frame=excluded.first_frame,
 last_frame=excluded.last_frame, item_count=excluded.item_count, digest=excluded.digest,
 param_version=excluded.param_version, algo_version=excluded.algo_version, error='', updated_at=excluded.updated_at`,
		seq, kind, firstFrame, lastFrame, len(items), digest, paramVersion, algoVersion, now)
	if err != nil {
		return core.WithBatch(core.NewError(core.CodeTxn, "commit batch row %d: %v", seq, err), seq)
	}
	for _, kv := range [][2]string{
		{"root_hash", rootHash}, {"param_version", paramVersion}, {"algo_version", algoVersion},
		{"params_json", paramsJSON},
	} {
		_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, kv[0], kv[1])
		if err != nil {
			return core.WithBatch(core.NewError(core.CodeTxn, "write meta batch %d: %v", seq, err), seq)
		}
	}
	if err := tx.Commit(); err != nil {
		return core.WithBatch(core.NewError(core.CodeTxn, "commit transaction batch %d: %v", seq, err), seq)
	}
	return nil
}

func (s *Store) FirstUnfinishedBatch() (int, bool, error) {
	var seq int
	err := s.db.QueryRow(`SELECT seq FROM batches WHERE status != 'committed' ORDER BY seq LIMIT 1`).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

func (s *Store) LoadCurrent(rootHash, paramVersion, algoVersion string, contentHashes map[int]string) (map[string]StoredEvidence, error) {
	ok, reason, err := s.SnapshotMatches(rootHash, paramVersion, algoVersion)
	if err != nil {
		return nil, err
	}
	if !ok {
		if err := s.Reset(reason); err != nil {
			return nil, err
		}
		return map[string]StoredEvidence{}, nil
	}
	rows, err := s.db.Query(`
SELECT e.cache_key,e.frame_index,e.evidence_type,e.content_hash,e.param_hash,e.algo_version,
       e.payload_json,e.digest,e.batch_seq
FROM current_evidence c JOIN evidence e ON e.cache_key=c.cache_key
ORDER BY c.frame_index,c.evidence_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]StoredEvidence{}
	mismatchReason := ""
	for rows.Next() {
		var ev StoredEvidence
		var payload string
		if err := rows.Scan(&ev.CacheKey, &ev.Frame, &ev.Type, &ev.ContentHash, &ev.ParamHash,
			&ev.AlgorithmVersion, &payload, &ev.Digest, &ev.BatchSeq); err != nil {
			return nil, err
		}
		ev.PayloadJSON = []byte(payload)
		if expectedHash, ok := contentHashes[ev.Frame]; ok && expectedHash != ev.ContentHash {
			mismatchReason = "content_hash_mismatch"
			break
		}
		if core.SHA256Hex(ev.PayloadJSON) != ev.Digest {
			mismatchReason = "evidence_digest_mismatch"
			break
		}
		out[nodeKey(ev.Frame, ev.Type)] = ev
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if mismatchReason != "" {
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := s.Reset(mismatchReason); err != nil {
			return nil, err
		}
		return map[string]StoredEvidence{}, nil
	}
	return out, nil
}

func (s *Store) LogRecords() ([]LogRecord, error) {
	rows, err := s.db.Query(`SELECT ordinal,batch_seq,item_ordinal,frame_index,evidence_type,cache_key,digest,action FROM evidence_log ORDER BY ordinal`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogRecord
	for rows.Next() {
		var r LogRecord
		if err := rows.Scan(&r.Ordinal, &r.BatchSeq, &r.ItemOrdinal, &r.Frame, &r.Type, &r.CacheKey, &r.Digest, &r.Action); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MaxBatchSeq() (int, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM batches`).Scan(&seq); err != nil {
		return 0, err
	}
	return int(seq.Int64), nil
}

func batchDigest(items []Item) string {
	h := core.SHA256Hex([]byte(fmt.Sprintf("items=%d", len(items))))
	for _, item := range items {
		h = core.SHA256Hex([]byte(h + item.CacheKey + item.Digest))
	}
	return h
}

func nodeKey(frame int, evidenceType string) string {
	return fmt.Sprintf("%d:%s", frame, evidenceType)
}
