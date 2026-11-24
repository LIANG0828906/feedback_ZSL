package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Entry struct {
	CacheKey     string
	FrameNo      int
	EvidenceType string
	ContentHash  string
	ParamVersion string
	AlgoVersion  string
	UpstreamKeys []string
	Payload      any
	ReplacedKey  string
}

type StoredEntry struct {
	CacheKey      string
	FrameNo       int
	EvidenceType  string
	ContentHash   string
	ParamVersion  string
	AlgoVersion   string
	UpstreamKeys  string
	PayloadJSON   string
	PayloadSHA256 string
	Status        string
	UpdatedSeq    int64
}

type Batch struct {
	BatchID        string
	Kind           string
	ParamVersion   string
	AlgoVersion    string
	TruthFP        string
	ContentRoot    string
	ContentHash    string
	EvidenceDigest string
	RecomputeCount int
	HitCount       int
	Entries        []Entry
}

type Checkpoint struct {
	Seq            int64  `json:"seq"`
	BatchID        string `json:"batch_id"`
	Kind           string `json:"kind"`
	ParamVersion   string `json:"param_version"`
	AlgoVersion    string `json:"algo_version"`
	TruthFP        string `json:"truth_fingerprint"`
	ContentRoot    string `json:"content_root"`
	ContentHash    string `json:"content_hash"`
	EvidenceDigest string `json:"evidence_digest"`
	RecomputeCount int    `json:"recompute_count"`
	HitCount       int    `json:"hit_count"`
	CreatedNS      int64  `json:"created_ns"`
}

type BatchItem struct {
	Seq          int64  `json:"seq"`
	Ordinal      int    `json:"ordinal"`
	CacheKey     string `json:"cache_key"`
	FrameNo      int    `json:"frame_no"`
	EvidenceType string `json:"evidence_type"`
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = ":memory:"
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) init(ctx context.Context) error {
	pragmas := []string{
		`PRAGMA busy_timeout=5000`,
		`PRAGMA journal_mode=WAL`,
		`PRAGMA foreign_keys=ON`,
	}
	for _, pragma := range pragmas {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS run_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoints (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  batch_id TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  truth_fingerprint TEXT NOT NULL,
  content_root TEXT NOT NULL,
  content_hash TEXT NOT NULL DEFAULT '',
  evidence_digest TEXT NOT NULL,
  recompute_count INTEGER NOT NULL,
  hit_count INTEGER NOT NULL,
  created_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS evidence (
  cache_key TEXT PRIMARY KEY,
  frame_no INTEGER NOT NULL,
  evidence_type TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  upstream_keys TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  payload_sha256 TEXT NOT NULL,
  status TEXT NOT NULL,
  updated_seq INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS batch_items (
  seq INTEGER NOT NULL,
  ordinal INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  frame_no INTEGER NOT NULL,
  evidence_type TEXT NOT NULL,
  PRIMARY KEY(seq, ordinal)
);
CREATE INDEX IF NOT EXISTS idx_evidence_frame_type ON evidence(frame_no, evidence_type);
`)
	if err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `ALTER TABLE checkpoints ADD COLUMN content_hash TEXT NOT NULL DEFAULT ''`)
	return nil
}

func (s *Store) SetRunMeta(ctx context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_meta(key,value) VALUES(?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) GetRunMeta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM run_meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) ResetAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM batch_items`,
		`DELETE FROM evidence`,
		`DELETE FROM checkpoints`,
		`DELETE FROM run_meta`,
		`DELETE FROM sqlite_sequence WHERE name IN ('checkpoints')`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Get(ctx context.Context, key string) (*StoredEntry, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT cache_key,frame_no,evidence_type,content_hash,param_version,algo_version,
upstream_keys,payload_json,payload_sha256,status,updated_seq FROM evidence WHERE cache_key=? AND status='ok'`, key)
	var e StoredEntry
	err := row.Scan(&e.CacheKey, &e.FrameNo, &e.EvidenceType, &e.ContentHash, &e.ParamVersion, &e.AlgoVersion,
		&e.UpstreamKeys, &e.PayloadJSON, &e.PayloadSHA256, &e.Status, &e.UpdatedSeq)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &e, true, nil
}

func (s *Store) PutFailure(ctx context.Context, e Entry, code, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, _ := json.Marshal(map[string]string{"code": code, "message": message})
	up, _ := json.Marshal(e.UpstreamKeys)
	_, err := s.db.ExecContext(ctx, `INSERT INTO evidence
(cache_key,frame_no,evidence_type,content_hash,param_version,algo_version,upstream_keys,payload_json,payload_sha256,status,updated_seq)
VALUES(?,?,?,?,?,?,?,?,?,'failed',0)
ON CONFLICT(cache_key) DO UPDATE SET
 payload_json=excluded.payload_json, payload_sha256=excluded.payload_sha256,
 status='failed', updated_seq=0`,
		e.CacheKey, e.FrameNo, e.EvidenceType, e.ContentHash, e.ParamVersion, e.AlgoVersion,
		string(up), string(payload), sha256Hex(payload))
	return err
}

func (s *Store) SubmitBatch(ctx context.Context, b Batch) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	now := time.Now().UnixNano()
	digest := b.EvidenceDigest
	if digest == "" {
		digest = DigestBatch(b)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO checkpoints
(batch_id,kind,param_version,algo_version,truth_fingerprint,content_root,content_hash,evidence_digest,recompute_count,hit_count,created_ns)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, b.BatchID, b.Kind, b.ParamVersion, b.AlgoVersion, b.TruthFP, b.ContentRoot,
		b.ContentHash, digest, b.RecomputeCount, b.HitCount, now)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	for ordinal, e := range b.Entries {
		if e.ReplacedKey != "" && e.ReplacedKey != e.CacheKey {
			if _, err := tx.ExecContext(ctx, `DELETE FROM evidence WHERE cache_key=?`, e.ReplacedKey); err != nil {
				_ = tx.Rollback()
				return 0, err
			}
		}
		up, _ := json.Marshal(e.UpstreamKeys)
		payload, err := json.Marshal(e.Payload)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		sum := sha256Hex(payload)
		_, err = tx.ExecContext(ctx, `INSERT INTO evidence
(cache_key,frame_no,evidence_type,content_hash,param_version,algo_version,upstream_keys,payload_json,payload_sha256,status,updated_seq)
VALUES(?,?,?,?,?,?,?,?,?,'ok',?)
ON CONFLICT(cache_key) DO UPDATE SET
 frame_no=excluded.frame_no, evidence_type=excluded.evidence_type, content_hash=excluded.content_hash,
 param_version=excluded.param_version, algo_version=excluded.algo_version, upstream_keys=excluded.upstream_keys,
 payload_json=excluded.payload_json, payload_sha256=excluded.payload_sha256, status='ok', updated_seq=excluded.updated_seq`,
			e.CacheKey, e.FrameNo, e.EvidenceType, e.ContentHash, e.ParamVersion, e.AlgoVersion, string(up), string(payload), sum, seq)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO batch_items(seq,ordinal,cache_key,frame_no,evidence_type)
VALUES(?,?,?,?,?)`, seq, ordinal, e.CacheKey, e.FrameNo, e.EvidenceType); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

func (s *Store) Checkpoints(ctx context.Context) ([]Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,batch_id,kind,param_version,algo_version,truth_fingerprint,content_root,content_hash,
evidence_digest,recompute_count,hit_count,created_ns FROM checkpoints ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Checkpoint{}
	for rows.Next() {
		var c Checkpoint
		if err := rows.Scan(&c.Seq, &c.BatchID, &c.Kind, &c.ParamVersion, &c.AlgoVersion, &c.TruthFP, &c.ContentRoot, &c.ContentHash,
			&c.EvidenceDigest, &c.RecomputeCount, &c.HitCount, &c.CreatedNS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) BatchItems(ctx context.Context) ([]BatchItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq,ordinal,cache_key,frame_no,evidence_type FROM batch_items ORDER BY seq,ordinal`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BatchItem{}
	for rows.Next() {
		var it BatchItem
		if err := rows.Scan(&it.Seq, &it.Ordinal, &it.CacheKey, &it.FrameNo, &it.EvidenceType); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) Entries(ctx context.Context) ([]StoredEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cache_key,frame_no,evidence_type,content_hash,param_version,algo_version,
upstream_keys,payload_json,payload_sha256,status,updated_seq FROM evidence ORDER BY frame_no,evidence_type,cache_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StoredEntry{}
	for rows.Next() {
		var e StoredEntry
		if err := rows.Scan(&e.CacheKey, &e.FrameNo, &e.EvidenceType, &e.ContentHash, &e.ParamVersion, &e.AlgoVersion,
			&e.UpstreamKeys, &e.PayloadJSON, &e.PayloadSHA256, &e.Status, &e.UpdatedSeq); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}

func DigestBatch(b Batch) string {
	h := sha256.New()
	write := func(s string) { _, _ = h.Write([]byte(s)); _, _ = h.Write([]byte{0}) }
	write(b.BatchID)
	write(b.ParamVersion)
	write(b.AlgoVersion)
	for _, e := range b.Entries {
		payload, _ := json.Marshal(e.Payload)
		write(e.CacheKey)
		write(e.EvidenceType)
		write(fmt.Sprintf("%x", sha256.Sum256(payload)))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
