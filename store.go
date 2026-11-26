package dialoguecal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const checkpointTableSQL = `CREATE TABLE IF NOT EXISTS checkpoints (
	sequence INTEGER,
	session TEXT,
	interval_id TEXT,
	dialogue_id TEXT,
	segment_id TEXT,
	start_frame INTEGER,
	end_frame INTEGER,
	start_sample INTEGER,
	end_sample INTEGER,
	feature_key TEXT,
	calibration_key TEXT,
	upstream_key TEXT,
	content_hash TEXT,
	feature_digest TEXT,
	calibration_digest TEXT,
	source_version INTEGER,
	conversion_version TEXT,
	algorithm_version TEXT,
	consumed_offset INTEGER,
	payload TEXT
)`

type Store struct {
	mu      sync.Mutex
	db      *sql.DB
	path    string
	nextSeq int64
}

type StoredCheckpoint struct {
	Sequence int64
	Session  string
	Result   IntervalResult
	Payload  string
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, &Error{Code: CodeIO, Path: "store.path", Message: "empty checkpoint path"}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, &Error{Code: CodeIO, Path: path, Message: err.Error()}
	}
	db, err := sql.Open("dialoguecal-sqlite", path)
	if err != nil {
		return nil, &Error{Code: CodeIO, Path: path, Message: err.Error()}
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if _, err := db.ExecContext(context.Background(), checkpointTableSQL); err != nil {
		_ = db.Close()
		return nil, &Error{Code: CodeIO, Path: path, Message: "create checkpoint table: " + err.Error()}
	}
	rows, err := store.loadAll(context.Background())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	for _, cp := range rows {
		if cp.Sequence > store.nextSeq {
			store.nextSeq = cp.Sequence
		}
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

// CommitInterval writes every checkpoint field in one database/sql
// transaction. Store.mu makes sequence assignment and physical commit order
// stable even if callers race.
func (s *Store) CommitInterval(ctx context.Context, session string, result IntervalResult) (int64, error) {
	if s == nil || s.db == nil {
		return 0, &Error{Code: CodeIO, Path: "store", Message: "store is closed"}
	}
	if session == "" {
		return 0, &Error{Code: CodeSchema, Field: "session", Message: "empty checkpoint session"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, err := json.Marshal(result)
	if err != nil {
		return 0, &Error{Code: CodeDecode, Path: "checkpoint.payload", Message: err.Error()}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, &Error{Code: CodeIO, Path: s.path, Message: "begin checkpoint transaction: " + err.Error()}
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	seq, err := maxCheckpointSequence(ctx, tx)
	if err != nil {
		return 0, err
	}
	seq++
	result.CommitSequence = seq
	const deleteSQL = `DELETE FROM checkpoints WHERE session = ? AND interval_id = ?`
	if _, err := tx.ExecContext(ctx, deleteSQL, session, result.IntervalID); err != nil {
		return 0, &Error{Code: CodeIO, Path: s.path, Message: "delete old checkpoint: " + err.Error()}
	}
	const insertSQL = `INSERT INTO checkpoints (
		sequence, session, interval_id, dialogue_id, segment_id, start_frame, end_frame,
		start_sample, end_sample, feature_key, calibration_key, upstream_key, content_hash,
		feature_digest, calibration_digest, source_version, conversion_version,
		algorithm_version, consumed_offset, payload
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	_, err = tx.ExecContext(ctx, insertSQL,
		seq, session, result.IntervalID, result.DialogueID, result.SegmentID,
		int64(result.StartFrame), int64(result.EndFrame), result.StartSample, result.EndSample,
		result.FeatureKey, result.CalibrationKey, result.UpstreamKey, result.ContentHash,
		result.FeatureDigest, result.CalibrationDigest, int64(result.SourceVersion),
		result.ConversionVersion, result.AlgorithmVersion, result.ConsumedOffset, string(payload),
	)
	if err != nil {
		return 0, &Error{Code: CodeIO, Path: s.path, Message: "insert checkpoint: " + err.Error()}
	}
	if err := tx.Commit(); err != nil {
		return 0, &Error{Code: CodeIO, Path: s.path, Message: "commit checkpoint: " + err.Error()}
	}
	rollback = false
	s.nextSeq = seq
	return seq, nil
}

func maxCheckpointSequence(ctx context.Context, tx *sql.Tx) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT sequence FROM checkpoints`)
	if err != nil {
		return 0, &Error{Code: CodeIO, Path: "checkpoints.sequence", Message: err.Error()}
	}
	defer rows.Close()
	var max int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return 0, &Error{Code: CodeIO, Path: "checkpoints.sequence", Message: err.Error()}
		}
		if seq > max {
			max = seq
		}
	}
	if err := rows.Err(); err != nil {
		return 0, &Error{Code: CodeIO, Path: "checkpoints.sequence", Message: err.Error()}
	}
	return max, nil
}

func (s *Store) LoadCheckpoints(ctx context.Context, session string) ([]StoredCheckpoint, error) {
	if s == nil || s.db == nil {
		return nil, &Error{Code: CodeIO, Path: "store", Message: "store is closed"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM checkpoints WHERE session = ?`, session)
	if err != nil {
		return nil, &Error{Code: CodeIO, Path: s.path, Message: "query checkpoints: " + err.Error()}
	}
	defer rows.Close()
	return scanCheckpoints(rows)
}

func (s *Store) loadAll(ctx context.Context) ([]StoredCheckpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM checkpoints`)
	if err != nil {
		return nil, &Error{Code: CodeIO, Path: s.path, Message: "query checkpoints: " + err.Error()}
	}
	defer rows.Close()
	return scanCheckpoints(rows)
}

func scanCheckpoints(rows *sql.Rows) ([]StoredCheckpoint, error) {
	out := make([]StoredCheckpoint, 0)
	for rows.Next() {
		var (
			seq, startFrame, endFrame, startSample, endSample, sourceVersion, consumed int64
			session, intervalID, dialogueID, segmentID                                 string
			featureKey, calibrationKey, upstreamKey, contentHash                       string
			featureDigest, calibrationDigest, conversionVersion, algorithmVersion      string
			payload                                                                    string
		)
		if err := rows.Scan(
			&seq, &session, &intervalID, &dialogueID, &segmentID, &startFrame, &endFrame,
			&startSample, &endSample, &featureKey, &calibrationKey, &upstreamKey, &contentHash,
			&featureDigest, &calibrationDigest, &sourceVersion, &conversionVersion,
			&algorithmVersion, &consumed, &payload,
		); err != nil {
			return nil, &Error{Code: CodeIO, Path: "checkpoints", Message: "scan checkpoint: " + err.Error()}
		}
		var result IntervalResult
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return nil, &Error{Code: CodeDecode, Path: "checkpoints.payload", Message: err.Error()}
		}
		result.IntervalID = intervalID
		result.DialogueID = dialogueID
		result.SegmentID = segmentID
		result.StartFrame = int(startFrame)
		result.EndFrame = int(endFrame)
		result.StartSample = startSample
		result.EndSample = endSample
		result.FeatureKey = featureKey
		result.CalibrationKey = calibrationKey
		result.UpstreamKey = upstreamKey
		result.ContentHash = contentHash
		result.FeatureDigest = featureDigest
		result.CalibrationDigest = calibrationDigest
		result.SourceVersion = SchemaVersion(sourceVersion)
		result.ConversionVersion = conversionVersion
		result.AlgorithmVersion = algorithmVersion
		result.ConsumedOffset = consumed
		result.CommitSequence = seq
		out = append(out, StoredCheckpoint{Sequence: seq, Session: session, Result: result, Payload: payload})
	}
	if err := rows.Err(); err != nil {
		return nil, &Error{Code: CodeIO, Path: "checkpoints", Message: err.Error()}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}

func (s *Store) DeleteSession(ctx context.Context, session string) error {
	if s == nil || s.db == nil {
		return &Error{Code: CodeIO, Path: "store", Message: "store is closed"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE session = ?`, session); err != nil {
		return &Error{Code: CodeIO, Path: s.path, Message: err.Error()}
	}
	return nil
}

func (s *Store) String() string {
	return fmt.Sprintf("Store(%s, next=%d)", s.path, s.nextSeq)
}
