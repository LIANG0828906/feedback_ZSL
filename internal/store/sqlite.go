package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"samplingkernel/internal/errs"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Checkpoint struct {
	ProjectID      string
	ProjectVersion string
	LastSeq        int
	Traces         []TraceRecord
	Services       []ServiceRecord
}

type TraceRecord struct {
	TraceID       string
	Status        string
	LastEventSeq  int
	ReceivedCount int
	MergedCount   int
	ResultJSON    string
}

type ServiceRecord struct {
	WindowStartMS int64
	ServiceID     string
	LimitSpans    uint64
	ChargedSpans  uint64
	ForcedSpans   uint64
}

func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errs.Wrap(errs.CheckpointOpenFailed, "open sqlite checkpoint failed", err).WithDetail(path)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, mapCorruptOpen(err, path)
	}
	store := &Store{db: db}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func mapCorruptOpen(err error, path string) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "not a database") || strings.Contains(message, "malformed") ||
		strings.Contains(message, "disk image") || strings.Contains(message, "database disk image is malformed") {
		return errs.Wrap(errs.CheckpointTruncated, "checkpoint database is truncated or corrupt", err).AtPath(path)
	}
	return errs.Wrap(errs.CheckpointOpenFailed, "ping checkpoint database failed", err).AtPath(path)
}

func (s *Store) init(ctx context.Context) error {
	schema := `
CREATE TABLE IF NOT EXISTS checkpoint_meta (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	project_id TEXT NOT NULL,
	project_version TEXT NOT NULL,
	last_seq INTEGER NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS trace_state (
	trace_id TEXT PRIMARY KEY,
	project_version TEXT NOT NULL,
	status TEXT NOT NULL,
	last_event_seq INTEGER NOT NULL,
	received_count INTEGER NOT NULL,
	merged_count INTEGER NOT NULL,
	result_json TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS service_state (
	window_start_ms INTEGER NOT NULL,
	service_id TEXT NOT NULL,
	project_version TEXT NOT NULL,
	limit_spans INTEGER NOT NULL,
	charged_spans INTEGER NOT NULL,
	forced_spans INTEGER NOT NULL,
	updated_at TEXT NOT NULL DEFAULT (datetime('now')),
	PRIMARY KEY (window_start_ms, service_id)
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return errs.Wrap(errs.CheckpointTruncated, "initialize checkpoint schema failed", err)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Reset(ctx context.Context) error {
	for _, statement := range []string{
		`DELETE FROM trace_state`,
		`DELETE FROM service_state`,
		`DELETE FROM checkpoint_meta`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return errs.Wrap(errs.CheckpointWriteFailed, "reset checkpoint failed", err)
		}
	}
	return nil
}

func (s *Store) SaveProgress(ctx context.Context, checkpoint Checkpoint) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errs.Wrap(errs.CheckpointWriteFailed, "begin checkpoint transaction failed", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, trace := range checkpoint.Traces {
		_, err = tx.ExecContext(ctx, `
INSERT INTO trace_state(trace_id, project_version, status, last_event_seq, received_count, merged_count, result_json, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(trace_id) DO UPDATE SET
	project_version=excluded.project_version,
	status=excluded.status,
	last_event_seq=excluded.last_event_seq,
	received_count=excluded.received_count,
	merged_count=excluded.merged_count,
	result_json=excluded.result_json,
	updated_at=datetime('now')`,
			trace.TraceID, checkpoint.ProjectVersion, trace.Status, trace.LastEventSeq,
			trace.ReceivedCount, trace.MergedCount, trace.ResultJSON)
		if err != nil {
			return errs.Wrap(errs.CheckpointWriteFailed, "upsert trace_state failed", err).WithDetail(trace.TraceID)
		}
	}

	for _, service := range checkpoint.Services {
		_, err = tx.ExecContext(ctx, `
INSERT INTO service_state(window_start_ms, service_id, project_version, limit_spans, charged_spans, forced_spans, updated_at)
VALUES(?, ?, ?, ?, ?, ?, datetime('now'))
ON CONFLICT(window_start_ms, service_id) DO UPDATE SET
	project_version=excluded.project_version,
	limit_spans=excluded.limit_spans,
	charged_spans=excluded.charged_spans,
	forced_spans=excluded.forced_spans,
	updated_at=datetime('now')`,
			service.WindowStartMS, service.ServiceID, checkpoint.ProjectVersion,
			service.LimitSpans, service.ChargedSpans, service.ForcedSpans)
		if err != nil {
			return errs.Wrap(errs.CheckpointWriteFailed, "upsert service_state failed", err).WithDetail(service.ServiceID)
		}
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO checkpoint_meta(id, project_id, project_version, last_seq, updated_at)
VALUES(1, ?, ?, ?, datetime('now'))
ON CONFLICT(id) DO UPDATE SET
	project_id=excluded.project_id,
	project_version=excluded.project_version,
	last_seq=excluded.last_seq,
	updated_at=datetime('now')`,
		checkpoint.ProjectID, checkpoint.ProjectVersion, checkpoint.LastSeq)
	if err != nil {
		return errs.Wrap(errs.CheckpointWriteFailed, "update global checkpoint sequence failed", err)
	}

	if err := tx.Commit(); err != nil {
		return errs.Wrap(errs.CheckpointWriteFailed, "commit checkpoint transaction failed", err)
	}
	return nil
}

func (s *Store) Load(ctx context.Context, expectedVersion string) (*Checkpoint, error) {
	var integrity string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return nil, errs.Wrap(errs.CheckpointTruncated, "checkpoint integrity check failed", err)
	}
	if integrity != "ok" {
		return nil, errs.New(errs.CheckpointTruncated, "checkpoint integrity check returned "+integrity)
	}

	checkpoint := &Checkpoint{}
	var lastSeq int
	err := s.db.QueryRowContext(ctx, `SELECT project_id, project_version, last_seq FROM checkpoint_meta WHERE id=1`).
		Scan(&checkpoint.ProjectID, &checkpoint.ProjectVersion, &lastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.Wrap(errs.CheckpointReadFailed, "load checkpoint meta failed", err)
	}
	checkpoint.LastSeq = lastSeq
	if expectedVersion != "" && checkpoint.ProjectVersion != expectedVersion {
		return checkpoint, errs.New(errs.CheckpointVersionMismatch, "checkpoint project version does not match snapshot").
			WithVersion(expectedVersion).
			AtPath("checkpoint_meta.project_version").
			WithDetail(fmt.Sprintf("checkpoint=%s expected=%s last_seq=%d", checkpoint.ProjectVersion, expectedVersion, lastSeq))
	}

	rows, err := s.db.QueryContext(ctx, `SELECT trace_id, status, last_event_seq, received_count, merged_count, result_json FROM trace_state ORDER BY trace_id`)
	if err != nil {
		return nil, errs.Wrap(errs.CheckpointReadFailed, "load trace_state failed", err)
	}
	defer rows.Close()
	for rows.Next() {
		var record TraceRecord
		if err := rows.Scan(&record.TraceID, &record.Status, &record.LastEventSeq, &record.ReceivedCount, &record.MergedCount, &record.ResultJSON); err != nil {
			return nil, errs.Wrap(errs.CheckpointReadFailed, "scan trace_state failed", err)
		}
		checkpoint.Traces = append(checkpoint.Traces, record)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.CheckpointReadFailed, "iterate trace_state failed", err)
	}

	serviceRows, err := s.db.QueryContext(ctx, `SELECT window_start_ms, service_id, limit_spans, charged_spans, forced_spans FROM service_state ORDER BY window_start_ms, service_id`)
	if err != nil {
		return nil, errs.Wrap(errs.CheckpointReadFailed, "load service_state failed", err)
	}
	defer serviceRows.Close()
	for serviceRows.Next() {
		var record ServiceRecord
		if err := serviceRows.Scan(&record.WindowStartMS, &record.ServiceID, &record.LimitSpans, &record.ChargedSpans, &record.ForcedSpans); err != nil {
			return nil, errs.Wrap(errs.CheckpointReadFailed, "scan service_state failed", err)
		}
		checkpoint.Services = append(checkpoint.Services, record)
	}
	return checkpoint, nil
}

func (s *Store) CompletedTraceJSON(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT trace_id, result_json FROM trace_state WHERE status='complete' ORDER BY trace_id`)
	if err != nil {
		return nil, errs.Wrap(errs.CheckpointReadFailed, "query completed traces failed", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, errs.Wrap(errs.CheckpointReadFailed, "scan completed trace failed", err)
		}
		out[id] = payload
	}
	return out, rows.Err()
}
