package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"dialoguefit/model"
)

type SegmentRecord struct {
	DialogueID       string `json:"dialogue_id"`
	PhonemeIndex     int    `json:"phoneme_index"`
	PhonemeID        string `json:"phoneme_id"`
	CommitSeq        int64  `json:"commit_seq"`
	ContentHash      string `json:"content_hash"`
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	TruthFingerprint string `json:"truth_fingerprint"`
	State            string `json:"state"`
	EnvelopeDigest   string `json:"envelope_digest"`
	StressJSON       string `json:"stress_json,omitempty"`
	SegmentJSON      string `json:"segment_json,omitempty"`
	PayloadJSON      string `json:"payload_json"`
	CreatedAt        string `json:"created_at"`
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, model.CheckpointError("", "db_path", "create checkpoint directory: %v", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, model.CheckpointError("", "db", "open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, model.CheckpointError("", "db", "ping sqlite: %v", err)
	}
	s := &Store{db: db, path: path}
	if err := s.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) initSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
INSERT OR IGNORE INTO meta(key,value) VALUES('schema_version','2');
CREATE TABLE IF NOT EXISTS segment_checkpoints (
	dialogue_id TEXT NOT NULL,
	phoneme_index INTEGER NOT NULL,
	phoneme_id TEXT NOT NULL,
	commit_seq INTEGER NOT NULL UNIQUE,
	content_hash TEXT NOT NULL,
	param_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	truth_fingerprint TEXT NOT NULL,
	state TEXT NOT NULL,
	envelope_digest TEXT NOT NULL,
	stress_json TEXT NOT NULL DEFAULT '',
	segment_json TEXT NOT NULL DEFAULT '',
	payload_json BLOB NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY(dialogue_id, phoneme_index)
);
CREATE INDEX IF NOT EXISTS idx_segment_dialogue_order
	ON segment_checkpoints(dialogue_id, phoneme_index);
`)
	if err != nil {
		return model.CheckpointError("", "schema", "initialize checkpoint schema: %v", err)
	}
	return nil
}

// CommitSegment serializes all writers and commits one segment in one
// transaction. The deterministic CommitSeq keeps replay order stable.
func (s *Store) CommitSegment(ctx context.Context, rec SegmentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.CreatedAt == "" {
		rec.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.CheckpointError(rec.DialogueID, "transaction", "begin segment transaction: %v", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
INSERT INTO segment_checkpoints(
	dialogue_id, phoneme_index, phoneme_id, commit_seq, content_hash,
	param_version, algorithm_version, truth_fingerprint, state,
	envelope_digest, stress_json, segment_json, payload_json, created_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(dialogue_id, phoneme_index) DO UPDATE SET
	phoneme_id=excluded.phoneme_id,
	commit_seq=excluded.commit_seq,
	content_hash=excluded.content_hash,
	param_version=excluded.param_version,
	algorithm_version=excluded.algorithm_version,
	truth_fingerprint=excluded.truth_fingerprint,
	state=excluded.state,
	envelope_digest=excluded.envelope_digest,
	stress_json=excluded.stress_json,
	segment_json=excluded.segment_json,
	payload_json=excluded.payload_json,
	created_at=excluded.created_at`,
		rec.DialogueID, rec.PhonemeIndex, rec.PhonemeID, rec.CommitSeq, rec.ContentHash,
		rec.ParamVersion, rec.AlgorithmVersion, rec.TruthFingerprint, rec.State,
		rec.EnvelopeDigest, rec.StressJSON, rec.SegmentJSON, rec.PayloadJSON, rec.CreatedAt)
	if err != nil {
		return model.CheckpointError(rec.DialogueID, "segment_checkpoints", "commit phoneme %d: %v", rec.PhonemeIndex, err)
	}
	if err := tx.Commit(); err != nil {
		return model.CheckpointError(rec.DialogueID, "transaction", "commit phoneme %d: %v", rec.PhonemeIndex, err)
	}
	return nil
}

func (s *Store) LoadDialogueSegments(ctx context.Context, dialogueID string) ([]SegmentRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT dialogue_id, phoneme_index, phoneme_id, commit_seq, content_hash,
       param_version, algorithm_version, truth_fingerprint, state,
       envelope_digest, stress_json, segment_json, payload_json, created_at
FROM segment_checkpoints
WHERE dialogue_id=?
ORDER BY phoneme_index ASC`, dialogueID)
	if err != nil {
		return nil, model.CheckpointError(dialogueID, "query", "load segments: %v", err)
	}
	defer rows.Close()
	var out []SegmentRecord
	for rows.Next() {
		var rec SegmentRecord
		if err := rows.Scan(&rec.DialogueID, &rec.PhonemeIndex, &rec.PhonemeID, &rec.CommitSeq,
			&rec.ContentHash, &rec.ParamVersion, &rec.AlgorithmVersion, &rec.TruthFingerprint,
			&rec.State, &rec.EnvelopeDigest, &rec.StressJSON, &rec.SegmentJSON,
			&rec.PayloadJSON, &rec.CreatedAt); err != nil {
			return nil, model.CheckpointError(dialogueID, "scan", "load segment row: %v", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) LoadAllOrdered(ctx context.Context) ([]SegmentRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT dialogue_id, phoneme_index, phoneme_id, commit_seq, content_hash,
       param_version, algorithm_version, truth_fingerprint, state,
       envelope_digest, stress_json, segment_json, payload_json, created_at
FROM segment_checkpoints
ORDER BY commit_seq ASC`)
	if err != nil {
		return nil, model.CheckpointError("", "query", "load all segments: %v", err)
	}
	defer rows.Close()
	var out []SegmentRecord
	for rows.Next() {
		var rec SegmentRecord
		if err := rows.Scan(&rec.DialogueID, &rec.PhonemeIndex, &rec.PhonemeID, &rec.CommitSeq,
			&rec.ContentHash, &rec.ParamVersion, &rec.AlgorithmVersion, &rec.TruthFingerprint,
			&rec.State, &rec.EnvelopeDigest, &rec.StressJSON, &rec.SegmentJSON,
			&rec.PayloadJSON, &rec.CreatedAt); err != nil {
			return nil, model.CheckpointError("", "scan", "load ordered segment row: %v", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) DeleteDialogue(ctx context.Context, dialogueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM segment_checkpoints WHERE dialogue_id=?`, dialogueID)
	if err != nil {
		return model.CheckpointError(dialogueID, "segment_checkpoints", "invalidate dialogue snapshot: %v", err)
	}
	return nil
}

func JSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"marshal_error":%q}`, err.Error())
	}
	return string(b)
}
