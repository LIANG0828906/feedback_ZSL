package syncgame

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNoCheckpoint = errors.New("no checkpoint")

type Checkpoint struct {
	RunID                string          `json:"run_id"`
	WindowIndex          int             `json:"window_index"`
	StartTick            int             `json:"start_tick"`
	EndTick              int             `json:"end_tick"`
	Tick                 int             `json:"tick"`
	Seed                 uint64          `json:"seed"`
	AlgorithmVersion     string          `json:"algorithm_version"`
	StreamFingerprint    string          `json:"stream_fingerprint"`
	State                WorldState      `json:"state"`
	Trace                []WorldState    `json:"trace"`
	InputOffset          int             `json:"input_offset"`
	RandomCursor         int             `json:"random_cursor"`
	ClientConfirmedFrame int             `json:"client_confirmed_frame"`
	RollbackEvents       []RollbackEvent `json:"rollback_events,omitempty"`
	SnapshotHash         string          `json:"snapshot_hash"`
}

func (c Checkpoint) ComputeHash() string {
	copy := c
	copy.SnapshotHash = ""
	// RunID is a storage namespace, not snapshot content. Two runs of the same
	// fixture must therefore produce the same content hash and commit sequence.
	copy.RunID = ""
	return hashCanonical(copy)
}

func (c *Checkpoint) Seal() {
	c.SnapshotHash = c.ComputeHash()
}

func (c *Checkpoint) Validate() error {
	if c.RunID == "" {
		return &GameError{Code: CodeSnapshot, Message: "checkpoint run ID is empty"}
	}
	if c.WindowIndex < 0 || c.StartTick < 0 || c.EndTick < c.StartTick || c.Tick < c.EndTick {
		return &GameError{Code: CodeSnapshot, Message: "checkpoint window bounds are invalid", Tick: c.Tick}
	}
	if c.AlgorithmVersion == "" || c.StreamFingerprint == "" {
		return &GameError{Code: CodeSnapshot, Message: "checkpoint identity is incomplete", Tick: c.Tick}
	}
	if len(c.Trace) == 0 {
		return &GameError{Code: CodeSnapshot, Message: "checkpoint trace is empty", Tick: c.Tick}
	}
	for i := 1; i < len(c.Trace); i++ {
		if c.Trace[i].Tick != c.Trace[i-1].Tick+1 {
			return &GameError{Code: CodeSnapshot, Message: "checkpoint trace is not contiguous", Tick: c.Trace[i].Tick}
		}
	}
	last := c.Trace[len(c.Trace)-1]
	if last.Tick != c.Tick {
		return &GameError{Code: CodeSnapshot, Message: "checkpoint tick does not match trace tail", Tick: c.Tick}
	}
	if mismatch := compareWorldStates(c.Tick, c.State, last); mismatch != nil {
		mismatch.Code = CodeSnapshot
		mismatch.Message = "checkpoint state does not match trace tail"
		return mismatch
	}
	if c.RandomCursor < c.Tick || c.InputOffset < c.Tick {
		return &GameError{Code: CodeSnapshot, Message: "checkpoint cursors are behind the tick", Tick: c.Tick}
	}
	if c.SnapshotHash != c.ComputeHash() {
		return &GameError{Code: CodeSnapshot, Message: "checkpoint content hash mismatch", Tick: c.Tick}
	}
	return nil
}

type WindowStatus struct {
	RunID         string `json:"run_id"`
	WindowIndex   int    `json:"window_index"`
	StartTick     int    `json:"start_tick"`
	EndTick       int    `json:"end_tick"`
	Status        string `json:"status"`
	Attempt       int    `json:"attempt"`
	Error         string `json:"error,omitempty"`
	CommittedTick int    `json:"committed_tick,omitempty"`
	SnapshotHash  string `json:"snapshot_hash,omitempty"`
}

const (
	WindowCommitted = "committed"
	WindowFailed    = "failed"
	WindowRetrying  = "retrying"
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite path is empty")
	}
	if path != ":memory:" && !looksLikeSQLiteURI(path) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	dsn := path
	if path == ":memory:" {
		dsn = "file:syncgame?mode=memory&cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection is intentional: every checkpoint is serialized and
	// SQLite writes never interleave.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil && path != ":memory:" {
		_ = db.Close()
		return nil, err
	}
	store := &Store{db: db}
	if err := store.createSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) createSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS checkpoints (
  run_id TEXT NOT NULL,
  window_index INTEGER NOT NULL,
  start_tick INTEGER NOT NULL,
  end_tick INTEGER NOT NULL,
  tick INTEGER NOT NULL,
  seed TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  stream_fingerprint TEXT NOT NULL,
  state_json BLOB NOT NULL,
  trace_json BLOB NOT NULL,
  input_offset INTEGER NOT NULL,
  random_cursor INTEGER NOT NULL,
  client_confirmed_frame INTEGER NOT NULL,
  snapshot_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (run_id, window_index)
);
CREATE TABLE IF NOT EXISTS rollback_events (
  run_id TEXT NOT NULL,
  window_index INTEGER NOT NULL,
  seq INTEGER NOT NULL,
  event_json BLOB NOT NULL,
  PRIMARY KEY (run_id, window_index, seq)
);
CREATE TABLE IF NOT EXISTS windows (
  run_id TEXT NOT NULL,
  window_index INTEGER NOT NULL,
  start_tick INTEGER NOT NULL,
  end_tick INTEGER NOT NULL,
  status TEXT NOT NULL,
  attempt INTEGER NOT NULL DEFAULT 1,
  error TEXT NOT NULL DEFAULT '',
  committed_tick INTEGER NOT NULL DEFAULT 0,
  snapshot_hash TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, window_index)
);
CREATE INDEX IF NOT EXISTS checkpoints_run_tick ON checkpoints(run_id, tick);
`)
	return err
}

func (s *Store) SaveWindow(ctx context.Context, checkpoint Checkpoint) (bool, error) {
	if err := checkpoint.Validate(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var existingHash string
	err = tx.QueryRowContext(ctx,
		`SELECT snapshot_hash FROM checkpoints WHERE run_id=? AND window_index=?`,
		checkpoint.RunID, checkpoint.WindowIndex).Scan(&existingHash)
	if err == nil {
		if existingHash != checkpoint.SnapshotHash {
			return false, &GameError{
				Code:    CodeConflict,
				Message: "committed checkpoint cannot be rewritten",
				Tick:    checkpoint.Tick,
				Field:   "snapshot_hash",
				Left:    existingHash,
				Right:   checkpoint.SnapshotHash,
			}
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	var maxWindow, maxTick sql.NullInt64
	var latestSeed, latestAlgo, latestFingerprint sql.NullString
	err = tx.QueryRowContext(ctx, `
SELECT window_index, tick, seed, algorithm_version, stream_fingerprint
FROM checkpoints WHERE run_id=? ORDER BY window_index DESC LIMIT 1`,
		checkpoint.RunID).Scan(&maxWindow, &maxTick, &latestSeed, &latestAlgo, &latestFingerprint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if checkpoint.WindowIndex != int(maxWindow.Int64)+1 {
			return false, &GameError{
				Code:    CodeOrder,
				Message: "checkpoint windows must be committed in order",
				Tick:    checkpoint.Tick,
				Field:   "window_index",
				Left:    maxWindow.Int64 + 1,
				Right:   checkpoint.WindowIndex,
			}
		}
		if checkpoint.Tick <= int(maxTick.Int64) {
			return false, &GameError{
				Code:    CodeOrder,
				Message: "checkpoint ticks must increase",
				Tick:    checkpoint.Tick,
				Field:   "tick",
				Left:    maxTick.Int64,
				Right:   checkpoint.Tick,
			}
		}
		if latestSeed.String != strconv.FormatUint(checkpoint.Seed, 10) ||
			latestAlgo.String != checkpoint.AlgorithmVersion ||
			latestFingerprint.String != checkpoint.StreamFingerprint {
			return false, &GameError{
				Code:    CodeSnapshot,
				Message: "checkpoint identity differs from the current run",
				Tick:    checkpoint.Tick,
				Field:   "run_identity",
			}
		}
	} else if checkpoint.WindowIndex != 0 {
		return false, &GameError{
			Code:    CodeOrder,
			Message: "the first checkpoint must use window index 0",
			Tick:    checkpoint.Tick,
			Field:   "window_index",
			Left:    0,
			Right:   checkpoint.WindowIndex,
		}
	}

	stateJSON, err := json.Marshal(checkpoint.State)
	if err != nil {
		return false, err
	}
	traceJSON, err := json.Marshal(checkpoint.Trace)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO checkpoints
(run_id, window_index, start_tick, end_tick, tick, seed, algorithm_version,
 stream_fingerprint, state_json, trace_json, input_offset, random_cursor,
 client_confirmed_frame, snapshot_hash, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		checkpoint.RunID, checkpoint.WindowIndex, checkpoint.StartTick, checkpoint.EndTick,
		checkpoint.Tick, strconv.FormatUint(checkpoint.Seed, 10), checkpoint.AlgorithmVersion,
		checkpoint.StreamFingerprint, stateJSON, traceJSON, checkpoint.InputOffset,
		checkpoint.RandomCursor, checkpoint.ClientConfirmedFrame, checkpoint.SnapshotHash,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	for i, event := range checkpoint.RollbackEvents {
		eventJSON, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			return false, marshalErr
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO rollback_events(run_id, window_index, seq, event_json) VALUES (?,?,?,?)`,
			checkpoint.RunID, checkpoint.WindowIndex, i, eventJSON); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO windows(run_id, window_index, start_tick, end_tick, status, attempt, error,
 committed_tick, snapshot_hash)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(run_id, window_index) DO UPDATE SET
  start_tick=excluded.start_tick, end_tick=excluded.end_tick, status=excluded.status,
  error='', committed_tick=excluded.committed_tick, snapshot_hash=excluded.snapshot_hash`,
		checkpoint.RunID, checkpoint.WindowIndex, checkpoint.StartTick, checkpoint.EndTick,
		WindowCommitted, 1, "", checkpoint.Tick, checkpoint.SnapshotHash)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) RecordWindowFailure(ctx context.Context, runID string, index, startTick, endTick int, cause error) error {
	if cause == nil {
		cause = errors.New("unknown window failure")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT snapshot_hash FROM checkpoints WHERE run_id=? AND window_index=?`,
		runID, index).Scan(&hash)
	if err == nil {
		return &GameError{
			Code:    CodeConflict,
			Message: "committed window cannot be marked failed",
			Field:   "window_index",
			Left:    index,
			Right:   WindowCommitted,
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO windows(run_id, window_index, start_tick, end_tick, status, attempt, error)
VALUES (?,?,?,?,?,1,?)
ON CONFLICT(run_id, window_index) DO UPDATE SET
  status=CASE WHEN windows.status='committed' THEN windows.status ELSE 'failed' END,
  attempt=CASE WHEN windows.status='committed' THEN windows.attempt ELSE windows.attempt+1 END,
  error=CASE WHEN windows.status='committed' THEN windows.error ELSE excluded.error END`,
		runID, index, startTick, endTick, WindowFailed, cause.Error())
	return err
}

func (s *Store) RetryWindow(ctx context.Context, runID string, index int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
UPDATE windows SET status=?, error='', attempt=attempt+1
WHERE run_id=? AND window_index=? AND status=?`,
		WindowRetrying, runID, index, WindowFailed)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return &GameError{
			Code:    CodeConflict,
			Message: "only a failed window can be retried",
			Field:   "window_index",
			Left:    index,
		}
	}
	return nil
}

func (s *Store) LoadLatest(ctx context.Context, sample *Sample, runID string) (*Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoints, err := s.loadAllLocked(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(checkpoints) == 0 {
		return nil, ErrNoCheckpoint
	}
	expectedSeed := strconv.FormatUint(sample.Seed, 10)
	for _, checkpoint := range checkpoints {
		identityMismatch := checkpoint.Seed != sample.Seed ||
			checkpoint.AlgorithmVersion != sample.AlgorithmVersion ||
			checkpoint.StreamFingerprint != sample.StreamFingerprint()
		if identityMismatch {
			reason := fmt.Sprintf("window %d identity mismatch", checkpoint.WindowIndex)
			if err := s.invalidateLocked(ctx, runID); err != nil {
				return nil, err
			}
			return nil, &GameError{
				Code:    CodeSnapshot,
				Message: "all snapshots invalidated: " + reason,
				Tick:    checkpoint.Tick,
				Field:   "run_identity",
				Details: map[string]any{
					"expected_seed":   expectedSeed,
					"actual_seed":     strconv.FormatUint(checkpoint.Seed, 10),
					"expected_algo":   sample.AlgorithmVersion,
					"actual_algo":     checkpoint.AlgorithmVersion,
					"expected_stream": sample.StreamFingerprint(),
					"actual_stream":   checkpoint.StreamFingerprint,
					"invalidated_run": runID,
				},
			}
		}
		if err := checkpoint.Validate(); err != nil {
			if invalidateErr := s.invalidateLocked(ctx, runID); invalidateErr != nil {
				return nil, invalidateErr
			}
			return nil, err
		}
	}
	for i := 1; i < len(checkpoints); i++ {
		if checkpoints[i].WindowIndex != checkpoints[i-1].WindowIndex+1 ||
			checkpoints[i].Tick <= checkpoints[i-1].Tick {
			_ = s.invalidateLocked(ctx, runID)
			return nil, &GameError{
				Code:    CodeSnapshot,
				Message: "snapshot ordering is invalid; all snapshots invalidated",
				Tick:    checkpoints[i].Tick,
				Field:   "window_index",
			}
		}
	}
	latest := checkpoints[len(checkpoints)-1]
	return &latest, nil
}

func (s *Store) InvalidateRun(ctx context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateLocked(ctx, runID)
}

func (s *Store) invalidateLocked(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM rollback_events WHERE run_id=?`,
		`DELETE FROM checkpoints WHERE run_id=?`,
		`DELETE FROM windows WHERE run_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, runID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) loadAllLocked(ctx context.Context, runID string) ([]Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, window_index, start_tick, end_tick, tick, seed, algorithm_version,
       stream_fingerprint, state_json, trace_json, input_offset, random_cursor,
       client_confirmed_frame, snapshot_hash
FROM checkpoints WHERE run_id=? ORDER BY window_index ASC`, runID)
	if err != nil {
		return nil, err
	}
	var checkpoints []Checkpoint
	for rows.Next() {
		var checkpoint Checkpoint
		var seedText string
		var stateJSON, traceJSON []byte
		if err := rows.Scan(
			&checkpoint.RunID, &checkpoint.WindowIndex, &checkpoint.StartTick,
			&checkpoint.EndTick, &checkpoint.Tick, &seedText,
			&checkpoint.AlgorithmVersion, &checkpoint.StreamFingerprint,
			&stateJSON, &traceJSON, &checkpoint.InputOffset,
			&checkpoint.RandomCursor, &checkpoint.ClientConfirmedFrame,
			&checkpoint.SnapshotHash,
		); err != nil {
			return nil, err
		}
		seed, err := strconv.ParseUint(seedText, 10, 64)
		if err != nil {
			return nil, &GameError{Code: CodeSnapshot, Message: "checkpoint seed is corrupt", Tick: checkpoint.Tick}
		}
		checkpoint.Seed = seed
		if err := json.Unmarshal(stateJSON, &checkpoint.State); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(traceJSON, &checkpoint.Trace); err != nil {
			_ = rows.Close()
			return nil, err
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range checkpoints {
		eventRows, err := s.db.QueryContext(ctx, `
SELECT event_json FROM rollback_events WHERE run_id=? AND window_index=? ORDER BY seq ASC`,
			runID, checkpoints[i].WindowIndex)
		if err != nil {
			return nil, err
		}
		for eventRows.Next() {
			var eventJSON []byte
			if err := eventRows.Scan(&eventJSON); err != nil {
				_ = eventRows.Close()
				return nil, err
			}
			var event RollbackEvent
			if err := json.Unmarshal(eventJSON, &event); err != nil {
				_ = eventRows.Close()
				return nil, err
			}
			checkpoints[i].RollbackEvents = append(checkpoints[i].RollbackEvents, event)
		}
		if err := eventRows.Err(); err != nil {
			_ = eventRows.Close()
			return nil, err
		}
		if err := eventRows.Close(); err != nil {
			return nil, err
		}
	}
	return checkpoints, nil
}

func (s *Store) Windows(ctx context.Context, runID string) ([]WindowStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, window_index, start_tick, end_tick, status, attempt, error,
       committed_tick, snapshot_hash
FROM windows WHERE run_id=? ORDER BY window_index ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var statuses []WindowStatus
	for rows.Next() {
		var status WindowStatus
		if err := rows.Scan(&status.RunID, &status.WindowIndex, &status.StartTick,
			&status.EndTick, &status.Status, &status.Attempt, &status.Error,
			&status.CommittedTick, &status.SnapshotHash); err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}

func (s *Store) CheckpointCount(ctx context.Context, runID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints WHERE run_id=?`, runID).Scan(&count)
	return count, err
}

func SortRollbackEvents(events []RollbackEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Tick != events[j].Tick {
			return events[i].Tick < events[j].Tick
		}
		if events[i].ClientID != events[j].ClientID {
			return events[i].ClientID < events[j].ClientID
		}
		return events[i].Frame < events[j].Frame
	})
}

func looksLikeSQLiteURI(path string) bool {
	return len(path) >= 5 && path[:5] == "file:"
}
