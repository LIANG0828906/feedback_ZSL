// Package checkpoint persists deterministic simulation state in SQLite.
package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/engine"
	"github.com/liang/collector-backpressure/model"
)

const FormatVersion = "checkpoint-v1"

type Store struct {
	db   *sql.DB
	path string
}

type Checkpoint struct {
	ProjectID      string          `json:"project_id"`
	ProjectVersion string          `json:"project_version"`
	SchemaVersion  string          `json:"schema_version"`
	LastTick       int             `json:"last_committed_tick"`
	Snapshot       engine.Snapshot `json:"snapshot"`
	CommittedAt    time.Time       `json:"committed_at"`
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := inspectFile(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, bpderr.Wrap(bpderr.ECheckpointCorrupt, "checkpoint", err, "cannot open sqlite database")
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path}
	if err := store.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.health(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func inspectFile(path string) error {
	if path == "" || path == ":memory:" {
		return nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return bpderr.Wrap(bpderr.ECheckpointCorrupt, "checkpoint.path", err, "cannot stat checkpoint file")
	}
	if info.IsDir() {
		return bpderr.New(bpderr.ECheckpointCorrupt, "checkpoint.path", "checkpoint path is a directory").WithContext("path", path)
	}
	if info.Size() < 100 {
		return bpderr.New(bpderr.ECheckpointTruncated, "checkpoint.file", "checkpoint file is too short to be a complete SQLite database").
			WithContext("path", path).WithContext("size_bytes", info.Size())
	}
	if info.Size() >= 16 {
		header := make([]byte, 16)
		f, err := os.Open(path)
		if err != nil {
			return bpderr.Wrap(bpderr.ECheckpointCorrupt, "checkpoint.file", err, "cannot read checkpoint header")
		}
		defer f.Close()
		if _, err := f.Read(header); err != nil {
			return bpderr.Wrap(bpderr.ECheckpointTruncated, "checkpoint.file", err, "cannot read SQLite header")
		}
		if string(header) != "SQLite format 3\x00" {
			return bpderr.New(bpderr.ECheckpointTruncated, "checkpoint.header", "invalid SQLite header").
				WithContext("path", path).WithContext("header", string(header))
		}
	}
	return nil
}

func (s *Store) init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS agent_checkpoints (
			project_version TEXT NOT NULL,
			tick INTEGER NOT NULL,
			agent_id TEXT NOT NULL,
			inflight_bytes INTEGER NOT NULL,
			pending_count INTEGER NOT NULL,
			pending_json TEXT NOT NULL,
			state_json TEXT NOT NULL,
			PRIMARY KEY (project_version, tick, agent_id)
		)`,
		`CREATE TABLE IF NOT EXISTS partition_checkpoints (
			project_version TEXT NOT NULL,
			tick INTEGER NOT NULL,
			partition_id TEXT NOT NULL,
			written_bytes INTEGER NOT NULL,
			acked_bytes INTEGER NOT NULL,
			backlog_bytes INTEGER NOT NULL,
			state_json TEXT NOT NULL,
			PRIMARY KEY (project_version, tick, partition_id)
		)`,
		`CREATE TABLE IF NOT EXISTS commits (
			tick INTEGER PRIMARY KEY,
			project_id TEXT NOT NULL,
			project_version TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			committed_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS global_cursor (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			last_committed_tick INTEGER NOT NULL,
			project_id TEXT NOT NULL,
			project_version TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			committed_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return classifySQLiteError(err, "checkpoint.schema")
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO checkpoint_meta(key,value) VALUES('format_version',?)`, FormatVersion)
	return err
}

func (s *Store) health(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return classifySQLiteError(err, "checkpoint.integrity_check")
	}
	if result != "ok" {
		return bpderr.New(bpderr.ECheckpointTruncated, "checkpoint.integrity_check", "SQLite integrity check did not return ok").
			WithContext("result", result).WithContext("path", s.path)
	}
	return nil
}

func classifySQLiteError(err error, field string) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "malformed") || strings.Contains(message, "truncated") || strings.Contains(message, "disk image") || strings.Contains(message, "file is not a database") {
		return bpderr.Wrap(bpderr.ECheckpointTruncated, field, err, "checkpoint file is truncated or malformed")
	}
	return bpderr.Wrap(bpderr.ECheckpointCorrupt, field, err, "checkpoint SQLite operation failed")
}

func (s *Store) Close() error { return s.db.Close() }

// SaveSnapshot writes agent state first, partition state second, and only then
// advances the global committed tick in one atomic transaction.
func (s *Store) SaveSnapshot(ctx context.Context, p *model.Project, snapshot engine.Snapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return bpderr.Wrap(bpderr.ECheckpointCorrupt, "checkpoint.tx", err, "cannot begin checkpoint transaction")
	}
	defer tx.Rollback()

	for _, agent := range snapshot.Agents {
		pendingJSON, err := json.Marshal(agent.Pending)
		if err != nil {
			return bpderr.Wrap(bpderr.ECheckpointCorrupt, "agent.pending_json", err, "cannot encode pending queue")
		}
		stateJSON, err := json.Marshal(agent)
		if err != nil {
			return bpderr.Wrap(bpderr.ECheckpointCorrupt, "agent.state_json", err, "cannot encode agent state")
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO agent_checkpoints
			(project_version,tick,agent_id,inflight_bytes,pending_count,pending_json,state_json)
			VALUES(?,?,?,?,?,?,?)`,
			p.Version, snapshot.Tick, agent.AgentID, agent.InflightBytes, len(agent.Pending), string(pendingJSON), string(stateJSON),
		); err != nil {
			return classifySQLiteError(err, "agent_checkpoints")
		}
	}

	for _, partition := range snapshot.Partitions {
		stateJSON, err := json.Marshal(partition)
		if err != nil {
			return bpderr.Wrap(bpderr.ECheckpointCorrupt, "partition.state_json", err, "cannot encode partition state")
		}
		backlog := partition.WrittenBytes - partition.AckedBytes
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO partition_checkpoints
			(project_version,tick,partition_id,written_bytes,acked_bytes,backlog_bytes,state_json)
			VALUES(?,?,?,?,?,?,?)`,
			p.Version, snapshot.Tick, partition.PartitionID, partition.WrittenBytes, partition.AckedBytes, backlog, string(stateJSON),
		); err != nil {
			return classifySQLiteError(err, "partition_checkpoints")
		}
	}

	committedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO commits(tick,project_id,project_version,schema_version,committed_at)
		VALUES(?,?,?,?,?)`,
		snapshot.Tick, p.ID, p.Version, p.SchemaVersion, committedAt,
	); err != nil {
		return classifySQLiteError(err, "commits")
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO global_cursor(id,last_committed_tick,project_id,project_version,schema_version,committed_at)
		VALUES(1,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			last_committed_tick=excluded.last_committed_tick,
			project_id=excluded.project_id,
			project_version=excluded.project_version,
			schema_version=excluded.schema_version,
			committed_at=excluded.committed_at`,
		snapshot.Tick, p.ID, p.Version, p.SchemaVersion, committedAt,
	); err != nil {
		return classifySQLiteError(err, "global_cursor")
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(err, "checkpoint.commit")
	}
	return nil
}

func (s *Store) LoadLatest(ctx context.Context) (*Checkpoint, error) {
	var cp Checkpoint
	var committedAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT last_committed_tick,project_id,project_version,schema_version,committed_at
		FROM global_cursor WHERE id=1`).Scan(
		&cp.LastTick, &cp.ProjectID, &cp.ProjectVersion, &cp.SchemaVersion, &committedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, classifySQLiteError(err, "global_cursor")
	}
	if t, perr := time.Parse(time.RFC3339Nano, committedAt); perr == nil {
		cp.CommittedAt = t
	}
	cp.Snapshot.Tick = cp.LastTick

	rows, err := s.db.QueryContext(ctx, `SELECT state_json FROM agent_checkpoints WHERE project_version=? AND tick=? ORDER BY agent_id`, cp.ProjectVersion, cp.LastTick)
	if err != nil {
		return nil, classifySQLiteError(err, "agent_checkpoints")
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, classifySQLiteError(err, "agent_checkpoints.state_json")
		}
		var state engine.AgentSnapshot
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return nil, bpderr.Wrap(bpderr.ECheckpointCorrupt, "agent_checkpoints.state_json", err, "cannot decode agent checkpoint")
		}
		cp.Snapshot.Agents = append(cp.Snapshot.Agents, state)
	}
	rows.Close()

	pRows, err := s.db.QueryContext(ctx, `SELECT state_json FROM partition_checkpoints WHERE project_version=? AND tick=? ORDER BY partition_id`, cp.ProjectVersion, cp.LastTick)
	if err != nil {
		return nil, classifySQLiteError(err, "partition_checkpoints")
	}
	defer pRows.Close()
	for pRows.Next() {
		var raw string
		if err := pRows.Scan(&raw); err != nil {
			return nil, classifySQLiteError(err, "partition_checkpoints.state_json")
		}
		var state engine.PartitionSnapshot
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return nil, bpderr.Wrap(bpderr.ECheckpointCorrupt, "partition_checkpoints.state_json", err, "cannot decode partition checkpoint")
		}
		cp.Snapshot.Partitions = append(cp.Snapshot.Partitions, state)
	}
	return &cp, nil
}

func (s *Store) LoadMatching(ctx context.Context, p *model.Project, requireSchema bool) (*Checkpoint, error) {
	cp, err := s.LoadLatest(ctx)
	if err != nil {
		return nil, err
	}
	if cp == nil {
		return nil, nil
	}
	if cp.ProjectVersion != p.Version || cp.ProjectID != p.ID {
		return nil, bpderr.New(bpderr.ECheckpointVersion, "global_cursor.project_version", "checkpoint project version does not match input project").
			WithContext("expected_project_id", p.ID).WithContext("actual_project_id", cp.ProjectID).
			WithContext("expected_version", p.Version).WithContext("actual_version", cp.ProjectVersion)
	}
	if requireSchema && cp.SchemaVersion != p.SchemaVersion {
		return nil, bpderr.New(bpderr.ECheckpointVersion, "global_cursor.schema_version", "checkpoint schema version does not match input schema").
			WithContext("expected_schema", p.SchemaVersion).WithContext("actual_schema", cp.SchemaVersion)
	}
	return cp, nil
}

func (s *Store) Reset(ctx context.Context) error {
	tables := []string{"agent_checkpoints", "partition_checkpoints", "commits", "global_cursor"}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return classifySQLiteError(err, "reset."+table)
		}
	}
	return tx.Commit()
}
