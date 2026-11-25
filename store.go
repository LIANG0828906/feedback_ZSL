package lipsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

type CharInterval struct {
	Frame     int `json:"frame"`
	CharStart int `json:"char_start"`
	CharEnd   int `json:"char_end"`
}

type Checkpoint struct {
	DialogueID            string
	WindowIndex           int
	FrameStart            int
	FrameEnd              int
	Status                WindowState
	ContentHash           string
	QuantizationVersion   string
	AlgorithmVersion      string
	Frames                []FrameResult
	Dependencies          []FrameDependency
	CharIntervals         []CharInterval
	LipSequence           []LipCategory
	DepKeys               []string
	ConsumedPhonemeOffset int
	ConsumedSamples       int64
	PayloadHash           string
}

type CachedFrame struct {
	Dependency FrameDependency
	Frame      FrameResult
}

type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, &DomainError{Code: CodeStage, Field: "sqlite", Message: "open sqlite database", Cause: err.Error()}
	}
	// A single connection is intentional: checkpoint transactions are serial
	// and SQLite never observes interleaved writer state.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		_ = db.Close()
		return nil, &DomainError{Code: CodeStage, Field: "sqlite.pragma", Message: "configure sqlite", Cause: err.Error()}
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, &DomainError{Code: CodeStage, Field: "sqlite.pragma", Message: "enable WAL", Cause: err.Error()}
	}
	store := &Store{db: db, path: path}
	if err := store.createSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) createSchema() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoints (
			dialogue_id TEXT NOT NULL,
			window_index INTEGER NOT NULL,
			frame_start INTEGER NOT NULL,
			frame_end INTEGER NOT NULL,
			status TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			q_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			frames_json TEXT NOT NULL,
			char_intervals_json TEXT NOT NULL,
			lip_sequence_json TEXT NOT NULL,
			dep_keys_json TEXT NOT NULL,
			consumed_phoneme_offset INTEGER NOT NULL,
			consumed_samples INTEGER NOT NULL,
			payload_hash TEXT NOT NULL,
			PRIMARY KEY (dialogue_id, window_index)
		)`,
		`CREATE TABLE IF NOT EXISTS frame_cache (
			dialogue_id TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			frame INTEGER NOT NULL,
			dep_json TEXT NOT NULL,
			frame_json TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			q_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			PRIMARY KEY (dialogue_id, cache_key)
		)`,
		`CREATE INDEX IF NOT EXISTS frame_cache_by_frame ON frame_cache (dialogue_id, frame)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return &DomainError{Code: CodeStage, Field: "sqlite.schema", Message: "initialize sqlite schema", Cause: err.Error()}
		}
	}
	return nil
}

func checkpointPayloadHash(cp Checkpoint) (string, error) {
	return hashParts("checkpoint-payload/v1", cp.Frames, cp.CharIntervals, cp.LipSequence, cp.ConsumedPhonemeOffset, cp.ConsumedSamples), nil
}

// CommitWindow atomically writes one window checkpoint and its per-frame cache
// rows. The Store mutex plus MaxOpenConns(1) make shared writes serial.
func (s *Store) CommitWindow(ctx context.Context, cp Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cp.Status != StateCommitted {
		return &DomainError{
			Code:       CodeState,
			DialogueID: cp.DialogueID,
			Window:     intPtr(cp.WindowIndex),
			Frame:      intPtr(cp.FrameStart),
			Current:    string(cp.Status),
			Expected:   string(StateCommitted),
			Reason:     "checkpoint requires committed window",
			Message:    "only committed windows may be checkpointed",
		}
	}
	if cp.FrameEnd-cp.FrameStart != len(cp.Frames) ||
		len(cp.CharIntervals) != len(cp.Frames) ||
		len(cp.LipSequence) != len(cp.Frames) ||
		len(cp.DepKeys) != len(cp.Frames) {
		return &DomainError{
			Code:       CodeStage,
			DialogueID: cp.DialogueID,
			Window:     intPtr(cp.WindowIndex),
			Frame:      intPtr(cp.FrameStart),
			Field:      "checkpoint",
			Reason:     "checkpoint payload length mismatch",
			Message:    "checkpoint payload does not match its frame range",
		}
	}
	payloadHash, err := checkpointPayloadHash(cp)
	if err != nil {
		return err
	}
	cp.PayloadHash = payloadHash
	framesJSON, err := json.Marshal(cp.Frames)
	if err != nil {
		return err
	}
	charJSON, err := json.Marshal(cp.CharIntervals)
	if err != nil {
		return err
	}
	lipJSON, err := json.Marshal(cp.LipSequence)
	if err != nil {
		return err
	}
	depsJSON, err := json.Marshal(cp.DepKeys)
	if err != nil {
		return err
	}
	depJSONs := make([]string, len(cp.Frames))
	frameJSONs := make([]string, len(cp.Frames))
	for i := range cp.Frames {
		dep := FrameDependency{}
		if i < len(cp.Dependencies) {
			dep = cp.Dependencies[i]
		} else {
			dep = FrameDependency{
				DialogueID:       cp.DialogueID,
				Frame:            cp.FrameStart + i,
				QVersion:         cp.QuantizationVersion,
				AlgorithmVersion: cp.AlgorithmVersion,
				CacheKey:         cp.DepKeys[i],
			}
		}
		depBytes, _ := json.Marshal(dep)
		frameBytes, _ := json.Marshal(cp.Frames[i])
		depJSONs[i] = string(depBytes)
		frameJSONs[i] = string(frameBytes)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return &DomainError{Code: CodeStage, DialogueID: cp.DialogueID, Window: intPtr(cp.WindowIndex), Field: "checkpoint", Message: "begin checkpoint transaction", Cause: err.Error()}
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO checkpoints (
		dialogue_id, window_index, frame_start, frame_end, status,
		content_hash, q_version, algorithm_version, frames_json,
		char_intervals_json, lip_sequence_json, dep_keys_json,
		consumed_phoneme_offset, consumed_samples, payload_hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(dialogue_id, window_index) DO UPDATE SET
		frame_start=excluded.frame_start, frame_end=excluded.frame_end,
		status=excluded.status, content_hash=excluded.content_hash,
		q_version=excluded.q_version, algorithm_version=excluded.algorithm_version,
		frames_json=excluded.frames_json, char_intervals_json=excluded.char_intervals_json,
		lip_sequence_json=excluded.lip_sequence_json, dep_keys_json=excluded.dep_keys_json,
		consumed_phoneme_offset=excluded.consumed_phoneme_offset,
		consumed_samples=excluded.consumed_samples, payload_hash=excluded.payload_hash`,
		cp.DialogueID, cp.WindowIndex, cp.FrameStart, cp.FrameEnd, string(StateCommitted),
		cp.ContentHash, cp.QuantizationVersion, cp.AlgorithmVersion, string(framesJSON),
		string(charJSON), string(lipJSON), string(depsJSON),
		cp.ConsumedPhonemeOffset, cp.ConsumedSamples, cp.PayloadHash)
	if err != nil {
		return &DomainError{Code: CodeStage, DialogueID: cp.DialogueID, Window: intPtr(cp.WindowIndex), Field: "checkpoint", Message: "write checkpoint", Cause: err.Error()}
	}
	for i := range cp.Frames {
		_, err = tx.ExecContext(ctx, `INSERT INTO frame_cache (
			dialogue_id, cache_key, frame, dep_json, frame_json,
			content_hash, q_version, algorithm_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(dialogue_id, cache_key) DO UPDATE SET
			frame=excluded.frame, dep_json=excluded.dep_json, frame_json=excluded.frame_json,
			content_hash=excluded.content_hash, q_version=excluded.q_version,
			algorithm_version=excluded.algorithm_version`,
			cp.DialogueID, cp.DepKeys[i], cp.FrameStart+i, depJSONs[i], frameJSONs[i],
			cp.ContentHash, cp.QuantizationVersion, cp.AlgorithmVersion)
		if err != nil {
			return &DomainError{Code: CodeStage, DialogueID: cp.DialogueID, Window: intPtr(cp.WindowIndex), Frame: intPtr(cp.FrameStart + i), Field: "frame_cache", Message: "write frame cache", Cause: err.Error()}
		}
	}
	if err := tx.Commit(); err != nil {
		return &DomainError{Code: CodeStage, DialogueID: cp.DialogueID, Window: intPtr(cp.WindowIndex), Field: "checkpoint", Message: "commit checkpoint transaction", Cause: err.Error()}
	}
	return nil
}

func (s *Store) LoadCheckpoints(ctx context.Context, dialogueID string) ([]Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT window_index, frame_start, frame_end, status,
		content_hash, q_version, algorithm_version, frames_json, char_intervals_json,
		lip_sequence_json, dep_keys_json, consumed_phoneme_offset, consumed_samples, payload_hash
		FROM checkpoints WHERE dialogue_id = ? ORDER BY window_index`, dialogueID)
	if err != nil {
		return nil, &DomainError{Code: CodeStage, DialogueID: dialogueID, Field: "checkpoint", Message: "read checkpoints", Cause: err.Error()}
	}
	defer rows.Close()
	checkpoints := make([]Checkpoint, 0)
	for rows.Next() {
		var cp Checkpoint
		var status string
		var framesJSON, charJSON, lipJSON, depsJSON string
		if err := rows.Scan(&cp.WindowIndex, &cp.FrameStart, &cp.FrameEnd, &status,
			&cp.ContentHash, &cp.QuantizationVersion, &cp.AlgorithmVersion,
			&framesJSON, &charJSON, &lipJSON, &depsJSON,
			&cp.ConsumedPhonemeOffset, &cp.ConsumedSamples, &cp.PayloadHash); err != nil {
			return nil, &DomainError{Code: CodeStage, DialogueID: dialogueID, Field: "checkpoint", Message: "scan checkpoint", Cause: err.Error()}
		}
		cp.DialogueID = dialogueID
		cp.Status = WindowState(status)
		if err := json.Unmarshal([]byte(framesJSON), &cp.Frames); err != nil {
			return nil, &DomainError{Code: CodeStage, DialogueID: dialogueID, Window: intPtr(cp.WindowIndex), Field: "checkpoint.frames_json", Message: "decode checkpoint frames", Cause: err.Error()}
		}
		if err := json.Unmarshal([]byte(charJSON), &cp.CharIntervals); err != nil {
			return nil, &DomainError{Code: CodeStage, DialogueID: dialogueID, Window: intPtr(cp.WindowIndex), Field: "checkpoint.char_intervals_json", Message: "decode checkpoint character intervals", Cause: err.Error()}
		}
		if err := json.Unmarshal([]byte(lipJSON), &cp.LipSequence); err != nil {
			return nil, &DomainError{Code: CodeStage, DialogueID: dialogueID, Window: intPtr(cp.WindowIndex), Field: "checkpoint.lip_sequence_json", Message: "decode checkpoint lip sequence", Cause: err.Error()}
		}
		if err := json.Unmarshal([]byte(depsJSON), &cp.DepKeys); err != nil {
			return nil, &DomainError{Code: CodeStage, DialogueID: dialogueID, Window: intPtr(cp.WindowIndex), Field: "checkpoint.dep_keys_json", Message: "decode checkpoint dependency keys", Cause: err.Error()}
		}
		checkpoints = append(checkpoints, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, &DomainError{Code: CodeStage, DialogueID: dialogueID, Field: "checkpoint", Message: "iterate checkpoints", Cause: err.Error()}
	}
	return checkpoints, nil
}

// ValidateCheckpoints enforces restart safety. A single mismatch rejects the
// whole set so the caller must discard it and recompute all frames.
func ValidateCheckpoints(p DialogueProject, checkpoints []Checkpoint, frameCount, windowSize int) (string, bool) {
	if windowSize <= 0 {
		windowSize = DefaultWindowSize
	}
	if len(checkpoints) == 0 {
		return "no_checkpoints", false
	}
	contentHash, err := ContentHash(p)
	if err != nil {
		return "content_hash_error", false
	}
	expectedWindows := (frameCount + windowSize - 1) / windowSize
	if len(checkpoints) > expectedWindows {
		return fmt.Sprintf("window_count_mismatch:%d/%d", len(checkpoints), expectedWindows), false
	}
	for i, cp := range checkpoints {
		expectedStart := i * windowSize
		expectedEnd := expectedStart + windowSize
		if expectedEnd > frameCount {
			expectedEnd = frameCount
		}
		if cp.WindowIndex != i || cp.FrameStart != expectedStart || cp.FrameEnd != expectedEnd {
			return fmt.Sprintf("window_range_mismatch:%d", i), false
		}
		if cp.Status != StateCommitted {
			return fmt.Sprintf("window_status_mismatch:%d", i), false
		}
		if cp.ContentHash != contentHash || cp.QuantizationVersion != p.Quantization.Version || cp.AlgorithmVersion != p.AlgorithmVersion {
			return fmt.Sprintf("hash_or_version_mismatch:%d", i), false
		}
		if len(cp.Frames) != expectedEnd-expectedStart || len(cp.CharIntervals) != len(cp.Frames) || len(cp.LipSequence) != len(cp.Frames) || len(cp.DepKeys) != len(cp.Frames) {
			return fmt.Sprintf("payload_length_mismatch:%d", i), false
		}
		for j, frame := range cp.Frames {
			if frame.Frame != expectedStart+j {
				return fmt.Sprintf("frame_number_mismatch:%d", i), false
			}
			if cp.CharIntervals[j].Frame != frame.Frame || cp.CharIntervals[j].CharStart != frame.CharStart || cp.CharIntervals[j].CharEnd != frame.CharEnd {
				return fmt.Sprintf("char_summary_mismatch:%d", i), false
			}
			if cp.LipSequence[j] != frame.Lip {
				return fmt.Sprintf("lip_sequence_mismatch:%d", i), false
			}
		}
		hash, err := checkpointPayloadHash(cp)
		if err != nil || hash != cp.PayloadHash {
			return fmt.Sprintf("payload_hash_mismatch:%d", i), false
		}
	}
	return "", true
}

func (s *Store) PurgeDialogue(ctx context.Context, dialogueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE dialogue_id = ?`, dialogueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM frame_cache WHERE dialogue_id = ?`, dialogueID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadFrameCache(ctx context.Context, dialogueID, cacheKey string) (CachedFrame, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var depJSON, frameJSON string
	err := s.db.QueryRowContext(ctx, `SELECT dep_json, frame_json FROM frame_cache
		WHERE dialogue_id = ? AND cache_key = ?`, dialogueID, cacheKey).Scan(&depJSON, &frameJSON)
	if err == sql.ErrNoRows {
		return CachedFrame{}, false, nil
	}
	if err != nil {
		return CachedFrame{}, false, &DomainError{Code: CodeStage, DialogueID: dialogueID, Field: "frame_cache", Message: "read frame cache", Cause: err.Error()}
	}
	var cached CachedFrame
	if err := json.Unmarshal([]byte(depJSON), &cached.Dependency); err != nil {
		return CachedFrame{}, false, err
	}
	if err := json.Unmarshal([]byte(frameJSON), &cached.Frame); err != nil {
		return CachedFrame{}, false, err
	}
	if cached.Dependency.CacheKey != cacheKey || cached.Frame.CacheKey != "" && cached.Frame.CacheKey != cacheKey {
		return CachedFrame{}, false, nil
	}
	cached.Frame.Reused = true
	cached.Frame.CacheKey = cacheKey
	return cached, true, nil
}

// ValidateFrameCacheForCheckpoints verifies that every cache row referenced by
// a committed checkpoint still has matching project hash, quantization version
// and algorithm version. A frame-cache mismatch invalidates the checkpoint set.
func (s *Store) ValidateFrameCacheForCheckpoints(ctx context.Context, checkpoints []Checkpoint) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cp := range checkpoints {
		for i, key := range cp.DepKeys {
			var frameJSON, contentHash, qVersion, algorithmVersion string
			err := s.db.QueryRowContext(ctx, `SELECT frame_json, content_hash, q_version, algorithm_version
				FROM frame_cache WHERE dialogue_id = ? AND cache_key = ?`, cp.DialogueID, key).Scan(
				&frameJSON, &contentHash, &qVersion, &algorithmVersion)
			if err == sql.ErrNoRows {
				return fmt.Sprintf("frame_cache_missing:%d:%d", cp.WindowIndex, i), false
			}
			if err != nil {
				return fmt.Sprintf("frame_cache_read_error:%d:%d", cp.WindowIndex, i), false
			}
			if contentHash != cp.ContentHash || qVersion != cp.QuantizationVersion || algorithmVersion != cp.AlgorithmVersion {
				return fmt.Sprintf("frame_cache_hash_or_version_mismatch:%d:%d", cp.WindowIndex, i), false
			}
			var cached FrameResult
			if err := json.Unmarshal([]byte(frameJSON), &cached); err != nil {
				return fmt.Sprintf("frame_cache_decode_error:%d:%d", cp.WindowIndex, i), false
			}
			if !cacheFrameMatches(cached, cp.Frames[i]) {
				return fmt.Sprintf("frame_cache_payload_mismatch:%d:%d", cp.WindowIndex, i), false
			}
		}
	}
	return "", true
}

func (s *Store) UpsertFrameCacheBatch(ctx context.Context, contentHash, qVersion, algorithmVersion string, entries []CachedFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, entry := range entries {
		depBytes, err := json.Marshal(entry.Dependency)
		if err != nil {
			return err
		}
		frameCopy := entry.Frame
		frameCopy.Reused = false
		frameCopy.CacheKey = entry.Dependency.CacheKey
		frameBytes, err := json.Marshal(frameCopy)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO frame_cache (
			dialogue_id, cache_key, frame, dep_json, frame_json,
			content_hash, q_version, algorithm_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(dialogue_id, cache_key) DO UPDATE SET
			frame=excluded.frame, dep_json=excluded.dep_json, frame_json=excluded.frame_json,
			content_hash=excluded.content_hash, q_version=excluded.q_version,
			algorithm_version=excluded.algorithm_version`,
			entry.Dependency.DialogueID, entry.Dependency.CacheKey, entry.Frame.Frame,
			string(depBytes), string(frameBytes), contentHash, qVersion, algorithmVersion)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
