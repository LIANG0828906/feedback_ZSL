// Package checkpoint persists recovery state in SQLite using an explicit
// stream-state-before-global-sequence commit order.
package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	_ "modernc.org/sqlite"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/engine"
	"multiplexgw/internal/model"
)

const (
	CheckpointVersion = 1
	DriverName        = "sqlite"
)

type ExpectedVersion struct {
	CheckpointVersion int
	SchemaVersion     string
	ProjectID         string
}

type Meta struct {
	CheckpointVersion int    `json:"checkpoint_version"`
	SchemaVersion     string `json:"schema_version"`
	ProjectID         string `json:"project_id"`
	ProjectHash       string `json:"project_hash,omitempty"`
}

type Store struct {
	db   *sql.DB
	Path string
}

// Open opens or creates a SQLite checkpoint and rejects malformed files.
func Open(path string) (*Store, error) {
	db, err := sql.Open(DriverName, path)
	if err != nil {
		return nil, bizerr.Wrap(bizerr.CheckpointCorrupt, path, err, "open sqlite checkpoint")
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, bizerr.Wrap(bizerr.CheckpointCorrupt, path, err, "ping sqlite checkpoint")
	}
	s := &Store{db: db, Path: path}
	if err := s.checkIntegrity(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) checkIntegrity() error {
	rows, err := s.db.Query(`PRAGMA integrity_check`)
	if err != nil {
		return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "integrity check query failed")
	}
	defer rows.Close()
	if !rows.Next() {
		return bizerr.New(bizerr.CheckpointCorrupt, s.Path, "integrity check returned no rows")
	}
	var out string
	if err := rows.Scan(&out); err != nil {
		return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "scan integrity check")
	}
	if out != "ok" {
		return bizerr.New(bizerr.CheckpointCorrupt, s.Path, "sqlite integrity_check=%s", out)
	}
	return nil
}

func (s *Store) initSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS stream_state (
			connection_id TEXT NOT NULL,
			stream_id INTEGER NOT NULL,
			event_index INTEGER NOT NULL,
			parent_id INTEGER NOT NULL,
			effective_parent_id INTEGER NOT NULL,
			weight_myriad INTEGER NOT NULL,
			window_remaining INTEGER NOT NULL,
			max_window INTEGER NOT NULL,
			sent_bytes INTEGER NOT NULL,
			state TEXT NOT NULL,
			blocked_since_ms INTEGER,
			reasons_json TEXT NOT NULL,
			reason_since_json TEXT NOT NULL,
			pending_json TEXT NOT NULL,
			runtime_json TEXT NOT NULL,
			PRIMARY KEY(connection_id, stream_id)
		)`,
		`CREATE TABLE IF NOT EXISTS scheduler_progress (
			connection_id TEXT PRIMARY KEY,
			progress_json TEXT NOT NULL,
			updated_event_index INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS connection_checkpoint (
			connection_id TEXT PRIMARY KEY,
			committed_event_seq INTEGER NOT NULL,
			connection_window INTEGER NOT NULL,
			updated_logical_ms INTEGER NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "initialize checkpoint schema")
		}
	}
	return nil
}

func (s *Store) WriteMeta(m Meta) error {
	if m.CheckpointVersion == 0 {
		m.CheckpointVersion = CheckpointVersion
	}
	tx, err := s.db.Begin()
	if err != nil {
		return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "begin meta tx")
	}
	for _, kv := range [][2]string{
		{"checkpoint_version", fmt.Sprint(m.CheckpointVersion)},
		{"schema_version", m.SchemaVersion},
		{"project_id", m.ProjectID},
		{"project_hash", m.ProjectHash},
	} {
		if _, err := tx.Exec(`INSERT INTO checkpoint_meta(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, kv[0], kv[1]); err != nil {
			_ = tx.Rollback()
			return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "write meta "+kv[0])
		}
	}
	return tx.Commit()
}

func (s *Store) LoadMeta() (*Meta, error) {
	rows, err := s.db.Query(`SELECT key,value FROM checkpoint_meta`)
	if err != nil {
		return nil, bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "load meta")
	}
	defer rows.Close()
	m := &Meta{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "scan meta")
		}
		switch k {
		case "checkpoint_version":
			fmt.Sscan(v, &m.CheckpointVersion)
		case "schema_version":
			m.SchemaVersion = v
		case "project_id":
			m.ProjectID = v
		case "project_hash":
			m.ProjectHash = v
		}
	}
	return m, nil
}

func (s *Store) CheckVersion(want ExpectedVersion) error {
	got, err := s.LoadMeta()
	if err != nil {
		return err
	}
	if got == nil || got.CheckpointVersion == 0 {
		return bizerr.New(bizerr.CheckpointVersion, s.Path, "checkpoint is missing version metadata")
	}
	if want.CheckpointVersion != 0 && got.CheckpointVersion != want.CheckpointVersion {
		return bizerr.New(bizerr.CheckpointVersion, s.Path,
			"checkpoint format version %d does not match supported %d", got.CheckpointVersion, want.CheckpointVersion).
			WithContext("got", got.CheckpointVersion).WithContext("want", want.CheckpointVersion)
	}
	if want.SchemaVersion != "" && got.SchemaVersion != want.SchemaVersion {
		return bizerr.New(bizerr.CheckpointVersion, s.Path,
			"checkpoint schema %q does not match expected %q", got.SchemaVersion, want.SchemaVersion).
			WithContext("got", got.SchemaVersion).WithContext("want", want.SchemaVersion)
	}
	if want.ProjectID != "" && got.ProjectID != "" && got.ProjectID != want.ProjectID {
		return bizerr.New(bizerr.CheckpointVersion, s.Path,
			"checkpoint project %q does not match expected %q", got.ProjectID, want.ProjectID)
	}
	return nil
}

type FaultPoint string

const (
	FaultNone            FaultPoint = ""
	FaultBeforeGlobalSeq FaultPoint = "before_global_seq"
	FaultAfterCommit     FaultPoint = "after_commit"
)

// SaveEngine persists one committed event. The final global sequence update is
// deliberately the last writing statement in the transaction.
func (s *Store) SaveEngine(connectionID string, e *engine.Engine, logicalMS int64, fault FaultPoint) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "begin checkpoint tx")
	}
	committed := e.CommittedThrough
	ids := append([]int(nil), e.Order...)
	sort.Ints(ids)
	for _, id := range ids {
		st := e.Streams[id]
		reasons := e.CurrentReasonsForCheckpoint(st)
		reasonsJSON, _ := json.Marshal(reasons)
		reasonJSON, _ := json.Marshal(st.ReasonSince)
		pendingJSON, _ := json.Marshal(st.Pending)
		runtimeJSON, _ := json.Marshal(st)
		var blockedSince any
		if len(st.ReasonSince) > 0 {
			min := int64(1 << 62)
			for _, t := range st.ReasonSince {
				if t < min {
					min = t
				}
			}
			blockedSince = min
		}
		if _, err := tx.Exec(`INSERT INTO stream_state
			(connection_id,stream_id,event_index,parent_id,effective_parent_id,weight_myriad,
			 window_remaining,max_window,sent_bytes,state,blocked_since_ms,reasons_json,reason_since_json,
			 pending_json,runtime_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(connection_id,stream_id) DO UPDATE SET
			 event_index=excluded.event_index,parent_id=excluded.parent_id,
			 effective_parent_id=excluded.effective_parent_id,weight_myriad=excluded.weight_myriad,
			 window_remaining=excluded.window_remaining,max_window=excluded.max_window,
			 sent_bytes=excluded.sent_bytes,state=excluded.state,
			 blocked_since_ms=excluded.blocked_since_ms,reasons_json=excluded.reasons_json,
			 reason_since_json=excluded.reason_since_json,
			 pending_json=excluded.pending_json,runtime_json=excluded.runtime_json`,
			connectionID, id, committed, st.ParentID, st.EffectiveParentID, st.WeightPermyriad,
			st.Window, st.MaxWindow, st.SentBytes, st.State, blockedSince,
			string(reasonsJSON), string(reasonJSON), string(pendingJSON), string(runtimeJSON)); err != nil {
			_ = tx.Rollback()
			return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "upsert stream state")
		}
	}
	progress := map[string]any{
		"current_weights":   currentWeights(e),
		"effective_parents": effectiveParents(e),
		"committed_through": committed,
	}
	progressJSON, _ := json.Marshal(progress)
	if _, err := tx.Exec(`INSERT INTO scheduler_progress(connection_id,progress_json,updated_event_index)
		VALUES(?,?,?) ON CONFLICT(connection_id) DO UPDATE SET
		progress_json=excluded.progress_json,updated_event_index=excluded.updated_event_index`,
		connectionID, string(progressJSON), committed); err != nil {
		_ = tx.Rollback()
		return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "upsert scheduler progress")
	}

	if fault == FaultBeforeGlobalSeq {
		// Abrupt termination before the global sequence is updated/committed.
		_ = syscallKillSelf()
		os.Exit(137)
	}

	if _, err := tx.Exec(`INSERT INTO connection_checkpoint
		(connection_id,committed_event_seq,connection_window,updated_logical_ms)
		VALUES(?,?,?,?)
		ON CONFLICT(connection_id) DO UPDATE SET
		 committed_event_seq=excluded.committed_event_seq,
		 connection_window=excluded.connection_window,
		 updated_logical_ms=excluded.updated_logical_ms`,
		connectionID, committed, e.ConnectionWindow, logicalMS); err != nil {
		_ = tx.Rollback()
		return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "update global event sequence")
	}
	if err := tx.Commit(); err != nil {
		return bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "commit checkpoint")
	}
	if fault == FaultAfterCommit {
		_ = syscallKillSelf()
		os.Exit(137)
	}
	return nil
}

type Recovered struct {
	CommittedSeq     int
	ConnectionWindow int64
	Engine           *engine.Engine
}

func (s *Store) LoadEngine(connectionID string, p *model.Project, opts engine.Options, want ExpectedVersion) (*engine.Engine, int, error) {
	if err := s.CheckVersion(want); err != nil {
		return nil, 0, err
	}
	var committed int
	var window int64
	err := s.db.QueryRow(`SELECT committed_event_seq,connection_window FROM connection_checkpoint WHERE connection_id=?`, connectionID).
		Scan(&committed, &window)
	if err == sql.ErrNoRows {
		// No committed event is a valid fresh restart.
		e, newErr := engine.New(p, opts)
		return e, -1, newErr
	}
	if err != nil {
		return nil, 0, bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "load global checkpoint")
	}
	e, err := engine.New(p, opts)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT runtime_json FROM stream_state WHERE connection_id=? ORDER BY stream_id`, connectionID)
	if err != nil {
		return nil, 0, bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "load stream states")
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, 0, bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "scan runtime json")
		}
		var st engine.StreamRuntime
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return nil, 0, bizerr.Wrap(bizerr.CheckpointCorrupt, s.Path, err, "decode runtime json")
		}
		e.Streams[st.ID] = &st
	}
	e.ConnectionWindow = window
	e.CommittedThrough = committed
	e.Traces = nil
	e.Sends = nil
	return e, committed, nil
}

func currentWeights(e *engine.Engine) map[int]int64 {
	out := map[int]int64{}
	for id, st := range e.Streams {
		out[id] = st.CurrentWeight
	}
	return out
}

func effectiveParents(e *engine.Engine) map[int]int {
	out := map[int]int{}
	for id, st := range e.Streams {
		out[id] = st.EffectiveParentID
	}
	return out
}
