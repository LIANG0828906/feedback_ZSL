// Package checkpoint persists simulation progress to a local SQLite database
// using a pure-Go driver (no CGO). Writes follow a strict order: all
// per-function states for a tick are committed first, and only afterwards is
// the global committed tick advanced inside the same transaction. A process
// killed at any point therefore never observes a half-finished tick.
package checkpoint

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"

	"coldkernel/internal/errcode"
	"coldkernel/internal/fncore"
)

// SchemaVersion is the checkpoint format version. A mismatch forces a full
// recomputation rather than a partial/unsafe restore.
const SchemaVersion = "checkpoint/v1"

// Store wraps one SQLite checkpoint database.
type Store struct {
	db            *sql.DB
	projectSource string
}

// Open opens (and if needed initialises) a checkpoint database at path.
// projectSource identifies the owning project snapshot; restores only accept
// checkpoints whose project and schema versions both match.
func Open(path, projectSource string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, errcode.New(errcode.ECheckpointState, "open failed", "path", path)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, errcode.New(errcode.ECheckpointState, "pragma failed", "path", path)
	}
	s := &Store{db: db, projectSource: projectSource}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS meta(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS committed_tick(
  id INTEGER PRIMARY KEY CHECK (id = 1),
  tick INTEGER NOT NULL,
  schema_version TEXT NOT NULL,
  project_source TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS function_state(
  tick INTEGER NOT NULL,
  function_id TEXT NOT NULL,
  state TEXT NOT NULL,
  PRIMARY KEY (tick, function_id)
);
CREATE TABLE IF NOT EXISTS tick_outcomes(
  tick INTEGER PRIMARY KEY,
  payload TEXT NOT NULL
);`)
	return err
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// FnCheckpoint is one function's persisted state for a tick.
type FnCheckpoint struct {
	FunctionID string
	State      *fncore.State
}

// Commit atomically writes all function states and then advances the global
// tick. The ordering inside the single transaction guarantees that a reader
// after recovery can never see the new global tick without all its states.
func (s *Store) Commit(tick int64, fns []FnCheckpoint, outcomesJSON []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return errcode.New(errcode.ECheckpointState, "begin failed", "tick", tick)
	}
	// Phase 1: per-function states first.
	for _, fn := range fns {
		raw, err := json.Marshal(fn.State)
		if err != nil {
			tx.Rollback()
			return errcode.New(errcode.ECheckpointState, "state encode failed",
				"function", fn.FunctionID)
		}
		if _, err := tx.Exec(
			`INSERT INTO function_state(tick, function_id, state)
			 VALUES(?, ?, ?)
			 ON CONFLICT(tick, function_id) DO UPDATE SET state=excluded.state`,
			tick, fn.FunctionID, string(raw)); err != nil {
			tx.Rollback()
			return errcode.New(errcode.ECheckpointState, "function state write failed",
				"function", fn.FunctionID)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO tick_outcomes(tick, payload) VALUES(?, ?)
		 ON CONFLICT(tick) DO UPDATE SET payload=excluded.payload`,
		tick, string(outcomesJSON)); err != nil {
		tx.Rollback()
		return errcode.New(errcode.ECheckpointState, "outcome write failed", "tick", tick)
	}
	// Phase 2: only now advance the global committed tick.
	if _, err := tx.Exec(
		`INSERT INTO committed_tick(id, tick, schema_version, project_source)
		 VALUES(1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET tick=excluded.tick,
		   schema_version=excluded.schema_version,
		   project_source=excluded.project_source`,
		tick, SchemaVersion, s.projectSource); err != nil {
		tx.Rollback()
		return errcode.New(errcode.ECheckpointState, "global tick write failed", "tick", tick)
	}
	if err := tx.Commit(); err != nil {
		return errcode.New(errcode.ECheckpointState, "commit failed", "tick", tick)
	}
	return nil
}

// LatestTick returns the highest durably committed tick plus the identity of
// the project that wrote it. A missing checkpoint yields tick -1.
func (s *Store) LatestTick() (int64, string, string, error) {
	var tick int64
	var schema, source string
	err := s.db.QueryRow(
		`SELECT tick, schema_version, project_source FROM committed_tick WHERE id=1`,
	).Scan(&tick, &schema, &source)
	if err == sql.ErrNoRows {
		return -1, "", "", nil
	}
	if err != nil {
		return -1, "", "", errcode.New(errcode.ECheckpointTruncated, "unreadable committed tick")
	}
	return tick, schema, source, nil
}

// Restore validates versions and returns the function states at tick. A
// schema or project mismatch is reported as E_CHECKPOINT_VERSION so callers
// recompute from scratch; missing/partial rows are E_CHECKPOINT_TRUNCATED.
func (s *Store) Restore(tick int64) (map[string]*fncore.State, []byte, error) {
	last, schema, source, err := s.LatestTick()
	if err != nil {
		return nil, nil, err
	}
	if last < 0 {
		return nil, nil, errcode.New(errcode.ECheckpointTruncated, "no checkpoint present")
	}
	if schema != SchemaVersion || source != s.projectSource {
		return nil, nil, errcode.New(errcode.ECheckpointVersion,
			"checkpoint identity mismatch",
			"got_schema", schema, "want_schema", SchemaVersion,
			"got_project", source, "want_project", s.projectSource)
	}
	if tick > last {
		return nil, nil, errcode.New(errcode.ECheckpointTruncated,
			"requested tick beyond commit point", "tick", tick, "committed", last)
	}
	rows, err := s.db.Query(
		`SELECT function_id, state FROM function_state WHERE tick=?`, tick)
	if err != nil {
		return nil, nil, errcode.New(errcode.ECheckpointTruncated, "state query failed")
	}
	defer rows.Close()
	states := map[string]*fncore.State{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, nil, errcode.New(errcode.ECheckpointTruncated, "state scan failed")
		}
		var st fncore.State
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return nil, nil, errcode.New(errcode.ECheckpointTruncated,
				"corrupt function state", "function", id)
		}
		states[id] = &st
	}
	var payload []byte
	err = s.db.QueryRow(`SELECT payload FROM tick_outcomes WHERE tick=?`, tick).Scan(&payload)
	if err != nil {
		return nil, nil, errcode.New(errcode.ECheckpointTruncated, "outcome row missing", "tick", tick)
	}
	return states, payload, nil
}

// Reset removes all rows so a forced full recomputation starts cleanly.
func (s *Store) Reset() error {
	for _, tbl := range []string{"function_state", "tick_outcomes", "committed_tick"} {
		if _, err := s.db.Exec(fmt.Sprintf("DELETE FROM %s", tbl)); err != nil {
			return errcode.New(errcode.ECheckpointState, "reset failed", "table", tbl)
		}
	}
	return nil
}
