package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"economycore/internal/core"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS checkpoints (
  scenario_id TEXT NOT NULL,
  window_index INTEGER NOT NULL,
  start_tick INTEGER NOT NULL,
  end_tick INTEGER NOT NULL,
  start_intent_offset INTEGER NOT NULL,
  end_intent_offset INTEGER NOT NULL,
  state_json BLOB NOT NULL,
  state_hash TEXT NOT NULL,
  params_version TEXT NOT NULL,
  scene_hash TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  committed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY (scenario_id, window_index)
);
CREATE TABLE IF NOT EXISTS window_failures (
  scenario_id TEXT NOT NULL,
  window_index INTEGER NOT NULL,
  start_tick INTEGER NOT NULL,
  end_tick INTEGER NOT NULL,
  code TEXT NOT NULL,
  message TEXT NOT NULL,
  retryable INTEGER NOT NULL,
  failed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY (scenario_id, window_index)
);
`

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, core.NewError(core.CodeParam, "checkpoint database path is empty").WithField("db")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, core.NewError(core.CodeIO, "failed to create checkpoint directory").At(dir).Wrap(err)
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, core.NewError(core.CodeIO, "failed to open SQLite checkpoint database").Wrap(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, core.NewError(core.CodeCheckpt, "failed to initialize checkpoint schema").Wrap(err)
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) CommitWindow(ctx context.Context, cp core.WindowCheckpoint) error {
	if err := cp.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.NewError(core.CodeCheckpt, "failed to begin checkpoint transaction").Wrap(err)
	}
	defer tx.Rollback()

	var lastIndex sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(window_index) FROM checkpoints WHERE scenario_id=?`, cp.ScenarioID,
	).Scan(&lastIndex); err != nil {
		return core.NewError(core.CodeCheckpt, "failed to read checkpoint tail").Wrap(err)
	}
	expected := 0
	if lastIndex.Valid {
		expected = int(lastIndex.Int64) + 1
	}
	if expected == 0 && (cp.StartTick != 0 || cp.StartIntentOffset != 0) {
		return core.NewError(core.CodeCheckpt, "first checkpoint must start at tick zero with offset zero").
			WithValues([]int64{cp.StartTick, int64(cp.StartIntentOffset)}, []int64{0, 0})
	}
	if cp.WindowIndex != expected {
		return core.NewError(core.CodeCheckpt, "checkpoint window is out of order").
			WithValues(cp.WindowIndex, expected)
	}
	if expected > 0 {
		var prevEnd, prevOffset int64
		if err := tx.QueryRowContext(ctx,
			`SELECT end_tick, end_intent_offset FROM checkpoints
			 WHERE scenario_id=? AND window_index=?`,
			cp.ScenarioID, cp.WindowIndex-1,
		).Scan(&prevEnd, &prevOffset); err != nil {
			return core.NewError(core.CodeCheckpt, "previous checkpoint is unavailable").Wrap(err)
		}
		if prevEnd != cp.StartTick || prevOffset != int64(cp.StartIntentOffset) {
			return core.NewError(core.CodeCheckpt, "checkpoint does not continue the committed prefix").
				WithValues([]int64{cp.StartTick, int64(cp.StartIntentOffset)}, []int64{prevEnd, prevOffset})
		}
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO checkpoints
		 (scenario_id, window_index, start_tick, end_tick, start_intent_offset,
		  end_intent_offset, state_json, state_hash, params_version, scene_hash, algorithm_version)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		cp.ScenarioID, cp.WindowIndex, cp.StartTick, cp.EndTick,
		cp.StartIntentOffset, cp.EndIntentOffset, cp.StateJSON, cp.StateHash,
		cp.ParamsVersion, cp.SceneHash, cp.AlgorithmVersion,
	)
	if err != nil {
		return core.NewError(core.CodeCheckpt, "failed to insert checkpoint").Wrap(err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM window_failures WHERE scenario_id=? AND window_index=?`,
		cp.ScenarioID, cp.WindowIndex,
	); err != nil {
		return core.NewError(core.CodeCheckpt, "failed to clear checkpoint failure").Wrap(err)
	}
	if err := tx.Commit(); err != nil {
		return core.NewError(core.CodeCheckpt, "failed to commit checkpoint transaction").Wrap(err)
	}
	return nil
}

func (s *Store) RecordFailure(ctx context.Context, f core.WindowFailure) error {
	if f.ScenarioID == "" || f.WindowIndex < 0 {
		return core.NewError(core.CodeCheckpt, "window failure identity is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO window_failures
		 (scenario_id, window_index, start_tick, end_tick, code, message, retryable)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(scenario_id, window_index) DO UPDATE SET
		   start_tick=excluded.start_tick, end_tick=excluded.end_tick,
		   code=excluded.code, message=excluded.message, retryable=excluded.retryable,
		   failed_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		f.ScenarioID, f.WindowIndex, f.StartTick, f.EndTick, f.Code, f.Message, boolInt(f.Retryable),
	)
	if err != nil {
		return core.NewError(core.CodeCheckpt, "failed to record window failure").Wrap(err)
	}
	return nil
}

func (s *Store) LoadResume(ctx context.Context, scene *core.Scenario) (core.ResumePoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT window_index, start_tick, end_tick, start_intent_offset,
		        end_intent_offset, state_json, state_hash, params_version,
		        scene_hash, algorithm_version
		 FROM checkpoints WHERE scenario_id=? ORDER BY window_index ASC`,
		scene.ID,
	)
	if err != nil {
		return core.ResumePoint{}, core.NewError(core.CodeCheckpt, "failed to query checkpoints").Wrap(err)
	}
	defer rows.Close()

	type row struct {
		index, startTick, endTick, startOffset, endOffset int64
		stateJSON                                         []byte
		stateHash, paramsVersion, sceneHash, algoVersion  string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.index, &r.startTick, &r.endTick, &r.startOffset,
			&r.endOffset, &r.stateJSON, &r.stateHash, &r.paramsVersion,
			&r.sceneHash, &r.algoVersion); err != nil {
			return core.ResumePoint{}, core.NewError(core.CodeCheckpt, "failed to scan checkpoint").Wrap(err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return core.ResumePoint{}, core.NewError(core.CodeCheckpt, "failed to iterate checkpoints").Wrap(err)
	}
	if err := rows.Close(); err != nil {
		return core.ResumePoint{}, core.NewError(core.CodeCheckpt, "failed to close checkpoint rows").Wrap(err)
	}
	if len(all) == 0 {
		return core.ResumePoint{NextWindow: 0}, nil
	}

	invalidReason := ""
	for i, r := range all {
		if r.index != int64(i) {
			invalidReason = "checkpoint window sequence is not contiguous"
			break
		}
		if r.paramsVersion != scene.ParamsVersion {
			invalidReason = "params_version mismatch"
			break
		}
		if r.sceneHash != scene.SceneHash {
			invalidReason = "scene_hash mismatch"
			break
		}
		if r.algoVersion != core.AlgorithmVersion {
			invalidReason = "algorithm_version mismatch"
			break
		}
		if i == 0 && (r.startTick != 0 || r.startOffset != 0) {
			invalidReason = "checkpoint prefix does not start at zero"
			break
		}
		if core.HashBytes(r.stateJSON) != r.stateHash {
			invalidReason = "checkpoint state hash mismatch"
			break
		}
		if i > 0 {
			prev := all[i-1]
			if prev.endTick != r.startTick || prev.endOffset != r.startOffset {
				invalidReason = "checkpoint prefix is not contiguous"
				break
			}
		}
	}
	if invalidReason != "" {
		if err := s.invalidateLocked(ctx, scene.ID); err != nil {
			return core.ResumePoint{}, err
		}
		return core.ResumePoint{NextWindow: 0, Reset: true}, nil
	}

	last := all[len(all)-1]
	state, err := core.DecodeState(last.stateJSON)
	if err != nil {
		if invalidErr := s.invalidateLocked(ctx, scene.ID); invalidErr != nil {
			return core.ResumePoint{}, invalidErr
		}
		return core.ResumePoint{NextWindow: 0, Reset: true}, nil
	}
	if state.Tick != last.endTick || state.IntentOffset != int(last.endOffset) {
		if invalidErr := s.invalidateLocked(ctx, scene.ID); invalidErr != nil {
			return core.ResumePoint{}, invalidErr
		}
		return core.ResumePoint{NextWindow: 0, Reset: true}, nil
	}
	if err := state.Validate(scene); err != nil {
		if invalidErr := s.invalidateLocked(ctx, scene.ID); invalidErr != nil {
			return core.ResumePoint{}, invalidErr
		}
		return core.ResumePoint{NextWindow: 0, Reset: true}, nil
	}
	return core.ResumePoint{
		State:      state,
		NextWindow: int(last.index) + 1,
		Found:      true,
	}, nil
}

func (s *Store) invalidateLocked(ctx context.Context, scenarioID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.NewError(core.CodeCheckpt, "failed to begin checkpoint invalidation").Wrap(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoints WHERE scenario_id=?`, scenarioID); err != nil {
		return core.NewError(core.CodeCheckpt, "failed to invalidate checkpoints").Wrap(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM window_failures WHERE scenario_id=?`, scenarioID); err != nil {
		return core.NewError(core.CodeCheckpt, "failed to invalidate window failures").Wrap(err)
	}
	if err := tx.Commit(); err != nil {
		return core.NewError(core.CodeCheckpt, "failed to commit checkpoint invalidation").Wrap(err)
	}
	return nil
}

func (s *Store) InvalidateAll(ctx context.Context, scenarioID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateLocked(ctx, scenarioID)
}

func (s *Store) Count(ctx context.Context, scenarioID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM checkpoints WHERE scenario_id=?`, scenarioID,
	).Scan(&n); err != nil {
		return 0, core.NewError(core.CodeCheckpt, "failed to count checkpoints").Wrap(err)
	}
	return n, nil
}

func (s *Store) Failure(ctx context.Context, scenarioID string, window int) (*core.WindowFailure, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var f core.WindowFailure
	var retry int
	err := s.db.QueryRowContext(ctx,
		`SELECT window_index, start_tick, end_tick, code, message, retryable
		 FROM window_failures WHERE scenario_id=? AND window_index=?`,
		scenarioID, window,
	).Scan(&f.WindowIndex, &f.StartTick, &f.EndTick, &f.Code, &f.Message, &retry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, core.NewError(core.CodeCheckpt, "failed to read window failure").Wrap(err)
	}
	f.ScenarioID = scenarioID
	f.Retryable = retry != 0
	return &f, nil
}

func (s *Store) ResetFailure(ctx context.Context, scenarioID string, window int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM window_failures WHERE scenario_id=? AND window_index=?`,
		scenarioID, window,
	)
	if err != nil {
		return core.NewError(core.CodeCheckpt, "failed to reset window failure").Wrap(err)
	}
	return nil
}

func (s *Store) String() string {
	return fmt.Sprintf("sqlite(%s)", s.path)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
