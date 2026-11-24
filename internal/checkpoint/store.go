package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"saveinterop/internal/core"
)

const EngineVersion = "checkpoint-v2"

type RunMetadata struct {
	RunID          string
	Mode           string
	SourceBranch   string
	TargetBranch   string
	MappingVersion string
	WindowSize     int
	InputHash      string
}

type WindowRecord struct {
	Index                 int
	ItemStart             int
	ItemEnd               int
	FingerprintDelta      string
	CumulativeFingerprint string
	ContentHash           string
	Payload               []byte
}

type Store struct {
	db     *sql.DB
	mu     sync.Mutex
	closed bool
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("checkpoint database path is required")
	}
	if !strings.HasPrefix(path, "file:") && path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	db, err := sql.Open("sqlite", path+separator+"_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA synchronous = FULL`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS checkpoint_runs (
	run_id TEXT PRIMARY KEY,
	mode TEXT NOT NULL,
	source_branch TEXT NOT NULL,
	target_branch TEXT NOT NULL,
	mapping_version TEXT NOT NULL,
	window_size INTEGER NOT NULL,
	input_hash TEXT NOT NULL,
	completed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoint_windows (
	run_id TEXT NOT NULL,
	window_index INTEGER NOT NULL,
	item_start INTEGER NOT NULL,
	item_end INTEGER NOT NULL,
	fp_delta TEXT NOT NULL,
	cumulative_fp TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	payload BLOB NOT NULL,
	PRIMARY KEY (run_id, window_index),
	FOREIGN KEY (run_id) REFERENCES checkpoint_runs(run_id) ON DELETE CASCADE
);
`); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// ValidateOrCreate either creates a fresh run or validates the entire existing
// run as one unit. A failure invalidates every window of that run.
func (s *Store) ValidateOrCreate(ctx context.Context, meta RunMetadata) ([]WindowRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("checkpoint store is closed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var mode, source, target, version, inputHash string
	var windowSize int
	err = tx.QueryRowContext(ctx, `
SELECT mode, source_branch, target_branch, mapping_version, window_size, input_hash
FROM checkpoint_runs WHERE run_id = ?`, meta.RunID).
		Scan(&mode, &source, &target, &version, &windowSize, &inputHash)
	if err == sql.ErrNoRows {
		_, err = tx.ExecContext(ctx, `
INSERT INTO checkpoint_runs
(run_id, mode, source_branch, target_branch, mapping_version, window_size, input_hash, completed, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			meta.RunID, meta.Mode, meta.SourceBranch, meta.TargetBranch,
			meta.MappingVersion, meta.WindowSize, meta.InputHash, time.Now().UnixMilli())
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if mode != meta.Mode || source != meta.SourceBranch || target != meta.TargetBranch ||
		version != meta.MappingVersion || windowSize != meta.WindowSize || inputHash != meta.InputHash {
		if err := deleteRunTx(ctx, tx, meta.RunID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, &core.Error{
			Code: core.CodeVersion, Op: "validate-checkpoint-run",
			Message: "checkpoint run metadata mismatch; run invalidated",
			Entry:   "checkpoint_runs.run_id", SourceBranch: meta.SourceBranch,
			TargetBranch: meta.TargetBranch, Expected: fmt.Sprintf("%s/%s/%s/%d/%s",
				meta.Mode, meta.SourceBranch, meta.TargetBranch, meta.WindowSize, meta.MappingVersion),
			Actual: fmt.Sprintf("%s/%s/%s/%d/%s", mode, source, target, windowSize, version),
			Window: 0,
		}
	}

	rows, err := tx.QueryContext(ctx, `
SELECT window_index, item_start, item_end, fp_delta, cumulative_fp, content_hash, payload
FROM checkpoint_windows WHERE run_id = ? ORDER BY window_index`, meta.RunID)
	if err != nil {
		return nil, err
	}
	var records []WindowRecord
	for rows.Next() {
		var record WindowRecord
		if err := rows.Scan(&record.Index, &record.ItemStart, &record.ItemEnd,
			&record.FingerprintDelta, &record.CumulativeFingerprint,
			&record.ContentHash, &record.Payload); err != nil {
			rows.Close()
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := validateRecords(meta, records); err != nil {
		if deleteErr := deleteRunTx(ctx, tx, meta.RunID); deleteErr != nil {
			return nil, deleteErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, commitErr
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Store) AppendWindow(ctx context.Context, meta RunMetadata, record WindowRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("checkpoint store is closed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var mode, source, target, version, inputHash string
	var windowSize int
	err = tx.QueryRowContext(ctx, `
SELECT mode, source_branch, target_branch, mapping_version, window_size, input_hash
FROM checkpoint_runs WHERE run_id = ?`, meta.RunID).
		Scan(&mode, &source, &target, &version, &windowSize, &inputHash)
	if err != nil {
		return err
	}
	if mode != meta.Mode || source != meta.SourceBranch || target != meta.TargetBranch ||
		version != meta.MappingVersion || windowSize != meta.WindowSize || inputHash != meta.InputHash {
		return &core.Error{
			Code: core.CodeVersion, Op: "append-checkpoint-window",
			Message:  "checkpoint metadata changed during run",
			Expected: meta.MappingVersion, Actual: version,
			Window: record.Index,
		}
	}

	var count int
	var lastEnd int
	var lastCumulative string
	err = tx.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MAX(item_end), 0) FROM checkpoint_windows WHERE run_id = ?`,
		meta.RunID).Scan(&count, &lastEnd)
	if err != nil {
		return err
	}
	if count > 0 {
		err = tx.QueryRowContext(ctx, `
SELECT cumulative_fp FROM checkpoint_windows
WHERE run_id = ? ORDER BY window_index DESC LIMIT 1`, meta.RunID).Scan(&lastCumulative)
		if err != nil {
			return err
		}
	}
	if record.Index != count || record.ItemStart != lastEnd {
		return &core.Error{
			Code: core.CodeVersion, Op: "append-checkpoint-window",
			Message:  "checkpoint windows must be written contiguously",
			Expected: fmt.Sprintf("index=%d,start=%d", count, lastEnd),
			Actual:   fmt.Sprintf("index=%d,start=%d", record.Index, record.ItemStart),
			Window:   record.Index,
		}
	}
	setRecordHash(meta, &record)
	if record.CumulativeFingerprint != NextCumulative(lastCumulative, record.FingerprintDelta) {
		return &core.Error{
			Code: core.CodeVersion, Op: "append-checkpoint-window",
			Message:  "cumulative fingerprint is not consistent with predecessor",
			Expected: NextCumulative(lastCumulative, record.FingerprintDelta),
			Actual:   record.CumulativeFingerprint,
			Window:   record.Index,
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO checkpoint_windows
(run_id, window_index, item_start, item_end, fp_delta, cumulative_fp, content_hash, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		meta.RunID, record.Index, record.ItemStart, record.ItemEnd,
		record.FingerprintDelta, record.CumulativeFingerprint, record.ContentHash, record.Payload)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Complete(ctx context.Context, meta RunMetadata, expectedWindows int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkpoint_windows WHERE run_id = ?`, meta.RunID).Scan(&count); err != nil {
		return err
	}
	if count != expectedWindows {
		return &core.Error{
			Code: core.CodeVersion, Op: "complete-checkpoint-run",
			Message:  "checkpoint window count mismatch",
			Expected: fmt.Sprintf("%d", expectedWindows), Actual: fmt.Sprintf("%d", count),
			Window: count,
		}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE checkpoint_runs SET completed = 1 WHERE run_id = ? AND completed = 0`, meta.RunID)
	return err
}

func (s *Store) Invalidate(ctx context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteRunTx(ctx, tx, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CountWindows(ctx context.Context, runID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkpoint_windows WHERE run_id = ?`, runID).Scan(&count)
	return count, err
}

func validateRecords(meta RunMetadata, records []WindowRecord) error {
	nextStart := 0
	previousCumulative := ""
	for i, record := range records {
		if record.Index != i {
			return &core.Error{
				Code: core.CodeVersion, Op: "validate-checkpoints",
				Message:  "checkpoint window sequence is not contiguous",
				Expected: fmt.Sprintf("%d", i), Actual: fmt.Sprintf("%d", record.Index),
				Window: i,
			}
		}
		if record.ItemStart != nextStart || record.ItemEnd <= record.ItemStart {
			return &core.Error{
				Code: core.CodeVersion, Op: "validate-checkpoints",
				Message:  "checkpoint item range is invalid or non-contiguous",
				Expected: fmt.Sprintf("start=%d", nextStart),
				Actual:   fmt.Sprintf("start=%d,end=%d", record.ItemStart, record.ItemEnd),
				Window:   i,
			}
		}
		expectedHash := ContentHash(meta, record.Index, record.ItemStart, record.ItemEnd,
			record.FingerprintDelta, record.Payload)
		if record.ContentHash != expectedHash {
			return &core.Error{
				Code: core.CodeVersion, Op: "validate-checkpoints",
				Message:  "checkpoint content hash mismatch",
				Expected: expectedHash, Actual: record.ContentHash, Window: i,
			}
		}
		expectedCumulative := NextCumulative(previousCumulative, record.FingerprintDelta)
		if record.CumulativeFingerprint != expectedCumulative {
			return &core.Error{
				Code: core.CodeVersion, Op: "validate-checkpoints",
				Message:  "checkpoint cumulative fingerprint mismatch",
				Expected: expectedCumulative, Actual: record.CumulativeFingerprint, Window: i,
			}
		}
		nextStart = record.ItemEnd
		previousCumulative = record.CumulativeFingerprint
	}
	return nil
}

func setRecordHash(meta RunMetadata, record *WindowRecord) {
	record.ContentHash = ContentHash(meta, record.Index, record.ItemStart, record.ItemEnd,
		record.FingerprintDelta, record.Payload)
}

func ContentHash(meta RunMetadata, index, start, end int, delta string, payload []byte) string {
	header := struct {
		RunID          string `json:"runId"`
		Mode           string `json:"mode"`
		SourceBranch   string `json:"sourceBranch"`
		TargetBranch   string `json:"targetBranch"`
		MappingVersion string `json:"mappingVersion"`
		WindowSize     int    `json:"windowSize"`
		InputHash      string `json:"inputHash"`
		Index          int    `json:"index"`
		Start          int    `json:"start"`
		End            int    `json:"end"`
		Delta          string `json:"delta"`
	}{
		RunID: meta.RunID, Mode: meta.Mode, SourceBranch: meta.SourceBranch,
		TargetBranch: meta.TargetBranch, MappingVersion: meta.MappingVersion,
		WindowSize: meta.WindowSize, InputHash: meta.InputHash,
		Index: index, Start: start, End: end, Delta: delta,
	}
	headerBytes, _ := json.Marshal(header)
	sumInput := append(append([]byte(nil), headerBytes...), payload...)
	return core.HashBytes(sumInput)
}

func NextCumulative(previous, delta string) string {
	return core.HashString(previous + "|" + delta)
}

func deleteRunTx(ctx context.Context, tx *sql.Tx, runID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_windows WHERE run_id = ?`, runID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_runs WHERE run_id = ?`, runID)
	return err
}
