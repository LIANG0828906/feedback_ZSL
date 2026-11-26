package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"meshpolicy/internal/xerr"
)

type WorkloadState struct {
	WorkloadID    string `json:"workload_id"`
	Namespace     string `json:"namespace"`
	AppliedSeq    int64  `json:"applied_seq"`
	DecisionsJSON string `json:"decisions_json"`
}

type QueueItem struct {
	MarkerID       string `json:"marker_id"`
	ChangeID       string `json:"change_id"`
	SourcePolicyID string `json:"source_policy_id"`
	Generation     int    `json:"generation"`
	Status         string `json:"status"`
	PayloadJSON    string `json:"payload_json"`
}

type Checkpoint struct {
	Path           string          `json:"path,omitempty"`
	ProjectVersion string          `json:"project_version"`
	CommittedSeq   int64           `json:"committed_seq"`
	Workloads      []WorkloadState `json:"workloads"`
	Queue          []QueueItem     `json:"queue"`
}

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	if path != ":memory:" && path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
			return nil, xerr.Wrap(xerr.ECheckpointOpen, "cannot create checkpoint directory", err, map[string]any{"path": path})
		}
		if err := preflightFile(path); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, xerr.Wrap(xerr.ECheckpointOpen, "cannot open sqlite checkpoint", err, map[string]any{"path": path})
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, classifySQLiteError(path, err)
	}
	st := &Store{db: db, path: path}
	if err := st.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

func preflightFile(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return xerr.Wrap(xerr.ECheckpointOpen, "cannot stat checkpoint", err, map[string]any{"path": path})
	}
	if info.IsDir() {
		return xerr.New(xerr.ECheckpointOpen, "checkpoint path is a directory", map[string]any{"path": path})
	}
	if info.Size() < 100 {
		return xerr.New(xerr.ECheckpointTruncated, "sqlite checkpoint is truncated below header/page metadata size", map[string]any{
			"path": path, "size_bytes": info.Size(), "minimum_header_bytes": 100,
		})
	}
	if info.Size() >= 16 {
		f, err := os.Open(path)
		if err != nil {
			return xerr.Wrap(xerr.ECheckpointOpen, "cannot read checkpoint header", err, map[string]any{"path": path})
		}
		header := make([]byte, 16)
		_, _ = f.Read(header)
		_ = f.Close()
		if string(header) != "SQLite format 3\x00" {
			return xerr.New(xerr.ECheckpointCorrupt, "checkpoint is not a sqlite database", map[string]any{"path": path})
		}
	}
	return nil
}

func classifySQLiteError(path string, err error) error {
	msg := err.Error()
	if strings.Contains(msg, "truncated") || strings.Contains(msg, "EOF") || strings.Contains(msg, "unexpected end") {
		return xerr.Wrap(xerr.ECheckpointTruncated, "checkpoint file is truncated", err, map[string]any{"path": path})
	}
	return xerr.Wrap(xerr.ECheckpointCorrupt, "checkpoint sqlite database is corrupt or unreadable", err, map[string]any{"path": path})
}

func (s *Store) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			id INTEGER PRIMARY KEY CHECK(id=1),
			project_version TEXT NOT NULL,
			committed_seq INTEGER NOT NULL,
			logical_updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`INSERT OR IGNORE INTO checkpoint_meta(id, project_version, committed_seq, logical_updated_at) VALUES(1, '', 0, 0)`,
		`CREATE TABLE IF NOT EXISTS workload_state (
			workload_id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			applied_seq INTEGER NOT NULL,
			decisions_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS propagation_queue (
			marker_id TEXT PRIMARY KEY,
			change_id TEXT NOT NULL,
			source_policy_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			status TEXT NOT NULL,
			payload_json TEXT NOT NULL
		)`,
	}
	for _, q := range statements {
		if _, err := s.db.Exec(q); err != nil {
			return xerr.Wrap(xerr.ECheckpointOpen, "cannot migrate checkpoint schema", err)
		}
	}
	return nil
}

type SaveOptions struct {
	// CrashAfterChildren simulates a process kill after workload/queue rows are written
	// inside the transaction but before the global sequence pointer update/commit.
	CrashAfterChildren bool
}

var ErrSimulatedCrash = errors.New("simulated process kill before checkpoint commit")

func (s *Store) SaveCheckpoint(cp Checkpoint) error {
	return s.SaveCheckpointWithOptions(cp, SaveOptions{})
}

func (s *Store) SaveCheckpointWithOptions(cp Checkpoint, opts SaveOptions) error {
	if cp.ProjectVersion == "" {
		return xerr.New(xerr.ERequestInvalid, "checkpoint project version is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return xerr.Wrap(xerr.ECheckpointOpen, "cannot begin checkpoint transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM workload_state`); err != nil {
		return xerr.Wrap(xerr.ECheckpointOpen, "cannot reset workload state", err)
	}
	for _, w := range cp.Workloads {
		if _, err := tx.Exec(`INSERT INTO workload_state(workload_id, namespace, applied_seq, decisions_json) VALUES(?,?,?,?)`,
			w.WorkloadID, w.Namespace, w.AppliedSeq, w.DecisionsJSON); err != nil {
			return xerr.Wrap(xerr.ECheckpointOpen, "cannot insert workload state", err, map[string]any{"workload_id": w.WorkloadID})
		}
	}

	if _, err := tx.Exec(`DELETE FROM propagation_queue`); err != nil {
		return xerr.Wrap(xerr.ECheckpointOpen, "cannot reset propagation queue", err)
	}
	for _, q := range cp.Queue {
		if _, err := tx.Exec(`INSERT INTO propagation_queue(marker_id, change_id, source_policy_id, generation, status, payload_json) VALUES(?,?,?,?,?,?)`,
			q.MarkerID, q.ChangeID, q.SourcePolicyID, q.Generation, q.Status, q.PayloadJSON); err != nil {
			return xerr.Wrap(xerr.ECheckpointOpen, "cannot insert propagation queue item", err, map[string]any{"marker_id": q.MarkerID})
		}
	}

	if opts.CrashAfterChildren {
		return ErrSimulatedCrash
	}

	// Commit point: only after workload and queue rows are staged do we move the global logical sequence.
	if _, err := tx.Exec(`UPDATE checkpoint_meta SET project_version=?, committed_seq=?, logical_updated_at=? WHERE id=1`,
		cp.ProjectVersion, cp.CommittedSeq, cp.CommittedSeq); err != nil {
		return xerr.Wrap(xerr.ECheckpointOpen, "cannot update global committed sequence", err)
	}
	if err := tx.Commit(); err != nil {
		return xerr.Wrap(xerr.ECheckpointOpen, "cannot commit checkpoint transaction", err)
	}
	return nil
}

func (s *Store) LoadCheckpoint() (*Checkpoint, error) {
	cp := &Checkpoint{Path: s.path}
	err := s.db.QueryRow(`SELECT project_version, committed_seq FROM checkpoint_meta WHERE id=1`).
		Scan(&cp.ProjectVersion, &cp.CommittedSeq)
	if err != nil {
		return nil, xerr.Wrap(xerr.ECheckpointCorrupt, "cannot read checkpoint pointer", err, map[string]any{"path": s.path})
	}
	rows, err := s.db.Query(`SELECT workload_id, namespace, applied_seq, decisions_json FROM workload_state ORDER BY workload_id`)
	if err != nil {
		return nil, xerr.Wrap(xerr.ECheckpointCorrupt, "cannot read workload state", err)
	}
	defer rows.Close()
	for rows.Next() {
		var w WorkloadState
		if err := rows.Scan(&w.WorkloadID, &w.Namespace, &w.AppliedSeq, &w.DecisionsJSON); err != nil {
			return nil, xerr.Wrap(xerr.ECheckpointCorrupt, "cannot scan workload state", err)
		}
		cp.Workloads = append(cp.Workloads, w)
	}
	if err := rows.Err(); err != nil {
		return nil, xerr.Wrap(xerr.ECheckpointCorrupt, "workload state iteration failed", err)
	}

	qrows, err := s.db.Query(`SELECT marker_id, change_id, source_policy_id, generation, status, payload_json FROM propagation_queue ORDER BY marker_id`)
	if err != nil {
		return nil, xerr.Wrap(xerr.ECheckpointCorrupt, "cannot read propagation queue", err)
	}
	defer qrows.Close()
	for qrows.Next() {
		var q QueueItem
		if err := qrows.Scan(&q.MarkerID, &q.ChangeID, &q.SourcePolicyID, &q.Generation, &q.Status, &q.PayloadJSON); err != nil {
			return nil, xerr.Wrap(xerr.ECheckpointCorrupt, "cannot scan queue item", err)
		}
		cp.Queue = append(cp.Queue, q)
	}
	if err := qrows.Err(); err != nil {
		return nil, xerr.Wrap(xerr.ECheckpointCorrupt, "queue iteration failed", err)
	}
	return cp, nil
}

func (s *Store) Close() error { return s.db.Close() }

func CheckVersion(cp *Checkpoint, expected string) error {
	if cp.ProjectVersion != expected {
		return xerr.New(xerr.ECheckpointVersionMismatch, "checkpoint project version does not match current engineering version", map[string]any{
			"checkpoint_version": cp.ProjectVersion, "expected_version": expected,
			"recovery": "discard checkpoint and recompute from logical start",
		})
	}
	return nil
}
