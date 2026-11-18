package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"archive/internal/model"
)

type SQLite struct {
	db *sql.DB
}

func OpenSQLite(path string) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_busy_timeout=10000&_journal_mode=WAL&_synchronous=FULL&_foreign_keys=ON", filepath.ToSlash(path))
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &SQLite{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) Close() error {
	return s.db.Close()
}

func (s *SQLite) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			action TEXT NOT NULL,
			status TEXT NOT NULL,
			stripe_id TEXT NOT NULL DEFAULT '',
			object_id TEXT NOT NULL DEFAULT '',
			dir_index INTEGER NOT NULL DEFAULT -1,
			reason TEXT NOT NULL DEFAULT '',
			details_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS audit_events_stripe ON audit_events(stripe_id, id)`,
		`CREATE TABLE IF NOT EXISTS repair_jobs (
			job_id TEXT PRIMARY KEY,
			stripe_id TEXT NOT NULL,
			object_id TEXT NOT NULL,
			missing_blocks TEXT NOT NULL,
			state TEXT NOT NULL,
			reserved_bytes INTEGER NOT NULL,
			day TEXT NOT NULL,
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			result_json TEXT NOT NULL DEFAULT '{}',
			metadata_applied INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS repair_jobs_stripe ON repair_jobs(stripe_id, started_at)`,
		`CREATE INDEX IF NOT EXISTS repair_jobs_state ON repair_jobs(state)`,
		`CREATE TABLE IF NOT EXISTS quota_ledger (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			day TEXT NOT NULL,
			job_id TEXT NOT NULL UNIQUE,
			stripe_id TEXT NOT NULL,
			bytes INTEGER NOT NULL,
			state TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS quota_ledger_day ON quota_ledger(day, state)`,
		`CREATE TABLE IF NOT EXISTS degradations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			stripe_id TEXT NOT NULL,
			object_id TEXT NOT NULL,
			dir_index INTEGER NOT NULL,
			action TEXT NOT NULL,
			reason TEXT NOT NULL,
			available_blocks INTEGER NOT NULL,
			status TEXT NOT NULL,
			details_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS degradations_stripe ON degradations(stripe_id, id)`,
		`CREATE TABLE IF NOT EXISTS reconciliations (
			id TEXT PRIMARY KEY,
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			missing_count INTEGER NOT NULL DEFAULT 0,
			length_mismatch_count INTEGER NOT NULL DEFAULT 0,
			digest_mismatch_count INTEGER NOT NULL DEFAULT 0,
			result_json TEXT NOT NULL DEFAULT '{}',
			error TEXT NOT NULL DEFAULT ''
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func (s *SQLite) QuotaUsage(ctx context.Context, day string, limit int64) (model.QuotaUsage, error) {
	var reserved, committed int64
	err := s.db.QueryRowContext(ctx,
		`SELECT
			COALESCE(SUM(CASE WHEN state='reserved' THEN bytes ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN state='committed' THEN bytes ELSE 0 END),0)
		 FROM quota_ledger WHERE day=? AND state IN ('reserved','committed')`, day).Scan(&reserved, &committed)
	if err != nil {
		return model.QuotaUsage{}, err
	}
	used := reserved + committed
	rem := limit - used
	if rem < 0 {
		rem = 0
	}
	return model.QuotaUsage{
		Day:            day,
		LimitBytes:     limit,
		UsedBytes:      used,
		ReservedBytes:  reserved,
		CommittedBytes: committed,
		RemainingBytes: rem,
	}, nil
}

func (s *SQLite) ReserveRepair(ctx context.Context, job model.RepairJob, limit int64, details string) (bool, model.QuotaUsage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, model.QuotaUsage{}, err
	}
	defer tx.Rollback()

	var existing int
	_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repair_jobs WHERE job_id=?`, job.JobID).Scan(&existing)
	if existing > 0 {
		usage, err := quotaUsageTx(ctx, tx, job.Day, limit)
		if err != nil {
			return false, model.QuotaUsage{}, err
		}
		return true, usage, tx.Commit()
	}

	usage, err := quotaUsageTx(ctx, tx, job.Day, limit)
	if err != nil {
		return false, model.QuotaUsage{}, err
	}
	if job.ReservedBytes < 0 {
		return false, usage, errors.New("negative reservation")
	}
	if usage.UsedBytes+job.ReservedBytes > limit {
		return false, usage, nil
	}
	missing, _ := json.Marshal(job.MissingBlocks)
	started := job.StartedAt.UTC().Format(time.RFC3339Nano)
	if job.StartedAt.IsZero() {
		started = nowRFC3339()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO repair_jobs(job_id,stripe_id,object_id,missing_blocks,state,reserved_bytes,day,started_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		job.JobID, job.StripeID, job.ObjectID, string(missing), "running", job.ReservedBytes, job.Day, started); err != nil {
		return false, usage, err
	}
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO quota_ledger(day,job_id,stripe_id,bytes,state,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		job.Day, job.JobID, job.StripeID, job.ReservedBytes, "reserved", now, now); err != nil {
		return false, usage, err
	}
	if err := insertAuditTx(ctx, tx, "repair", "reserved", job.StripeID, job.ObjectID, -1,
		"quota_reserved", details); err != nil {
		return false, usage, err
	}
	if err := tx.Commit(); err != nil {
		return false, usage, err
	}
	usage.UsedBytes += job.ReservedBytes
	usage.ReservedBytes += job.ReservedBytes
	usage.RemainingBytes = limit - usage.UsedBytes
	if usage.RemainingBytes < 0 {
		usage.RemainingBytes = 0
	}
	return true, usage, nil
}

func quotaUsageTx(ctx context.Context, tx *sql.Tx, day string, limit int64) (model.QuotaUsage, error) {
	var reserved, committed int64
	err := tx.QueryRowContext(ctx,
		`SELECT
			COALESCE(SUM(CASE WHEN state='reserved' THEN bytes ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN state='committed' THEN bytes ELSE 0 END),0)
		 FROM quota_ledger WHERE day=? AND state IN ('reserved','committed')`, day).Scan(&reserved, &committed)
	if err != nil {
		return model.QuotaUsage{}, err
	}
	used := reserved + committed
	rem := limit - used
	if rem < 0 {
		rem = 0
	}
	return model.QuotaUsage{Day: day, LimitBytes: limit, UsedBytes: used, ReservedBytes: reserved,
		CommittedBytes: committed, RemainingBytes: rem}, nil
}

func (s *SQLite) CompleteRepair(ctx context.Context, jobID, stripeID, objectID string, resultJSON, details string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM repair_jobs WHERE job_id=?`, jobID).Scan(&state); err != nil {
		return err
	}
	if state == "completed" {
		return tx.Commit()
	}
	if state != "running" {
		return fmt.Errorf("repair job %s is in state %s, cannot complete", jobID, state)
	}
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx,
		`UPDATE repair_jobs SET state='completed', finished_at=?, result_json=?, error='', metadata_applied=0
		 WHERE job_id=?`, now, resultJSON, jobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE quota_ledger SET state='committed', updated_at=? WHERE job_id=?`, now, jobID); err != nil {
		return err
	}
	if err := insertAuditTx(ctx, tx, "repair", "completed", stripeID, objectID, -1, "verification_ok", details); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) FailRepair(ctx context.Context, jobID, stripeID, objectID, reason, resultJSON, details string, releaseQuota bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM repair_jobs WHERE job_id=?`, jobID).Scan(&state); err != nil {
		return err
	}
	if state == "completed" {
		return nil
	}
	now := nowRFC3339()
	if _, err := tx.ExecContext(ctx,
		`UPDATE repair_jobs SET state='failed', finished_at=?, error=?, result_json=? WHERE job_id=?`,
		now, reason, resultJSON, jobID); err != nil {
		return err
	}
	ledgerState := "released"
	if releaseQuota {
		if _, err := tx.ExecContext(ctx,
			`UPDATE quota_ledger SET state=?, updated_at=? WHERE job_id=? AND state!='committed'`,
			ledgerState, now, jobID); err != nil {
			return err
		}
	}
	if err := insertAuditTx(ctx, tx, "repair", "failed", stripeID, objectID, -1, reason, details); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) MarkMetadataApplied(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repair_jobs SET metadata_applied=1 WHERE job_id=?`, jobID)
	return err
}

func (s *SQLite) ActiveRepairJobs(ctx context.Context) ([]model.RepairJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT job_id,stripe_id,object_id,missing_blocks,state,reserved_bytes,day,started_at,finished_at,error,result_json,metadata_applied
		 FROM repair_jobs WHERE state='running' ORDER BY started_at, job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *SQLite) CompletedUnappliedJobs(ctx context.Context) ([]model.RepairJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT job_id,stripe_id,object_id,missing_blocks,state,reserved_bytes,day,started_at,finished_at,error,result_json,metadata_applied
		 FROM repair_jobs WHERE state='completed' AND metadata_applied=0 ORDER BY finished_at, job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *SQLite) RepairJob(ctx context.Context, jobID string) (model.RepairJob, error) {
	var job model.RepairJob
	var missing, started, finished string
	var applied int
	err := s.db.QueryRowContext(ctx,
		`SELECT job_id,stripe_id,object_id,missing_blocks,state,reserved_bytes,day,started_at,finished_at,error,result_json,metadata_applied
		 FROM repair_jobs WHERE job_id=?`, jobID).Scan(&job.JobID, &job.StripeID, &job.ObjectID, &missing,
		&job.State, &job.ReservedBytes, &job.Day, &started, &finished, &job.Error, &job.ResultJSON, &applied)
	if err != nil {
		return job, err
	}
	_ = json.Unmarshal([]byte(missing), &job.MissingBlocks)
	job.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	job.FinishedAt, _ = time.Parse(time.RFC3339Nano, finished)
	job.MetadataDone = applied == 1
	return job, nil
}

func scanJobs(rows *sql.Rows) ([]model.RepairJob, error) {
	var out []model.RepairJob
	for rows.Next() {
		var job model.RepairJob
		var missing, started, finished string
		var applied int
		if err := rows.Scan(&job.JobID, &job.StripeID, &job.ObjectID, &missing, &job.State,
			&job.ReservedBytes, &job.Day, &started, &finished, &job.Error, &job.ResultJSON, &applied); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(missing), &job.MissingBlocks)
		job.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		job.FinishedAt, _ = time.Parse(time.RFC3339Nano, finished)
		job.MetadataDone = applied == 1
		out = append(out, job)
	}
	return out, rows.Err()
}

func (s *SQLite) RepairCounts(ctx context.Context) (inProgress, success, failure int64, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM repair_jobs WHERE state IN ('running','completed','failed') GROUP BY state`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int64
		if err = rows.Scan(&state, &n); err != nil {
			return
		}
		switch state {
		case "running":
			inProgress = n
		case "completed":
			success = n
		case "failed":
			failure = n
		}
	}
	err = rows.Err()
	return
}

func (s *SQLite) RecordDirEvent(ctx context.Context, action string, dirIndex int, reason string, strips []model.DegradationRecord, details string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowRFC3339()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO degradations(ts,stripe_id,object_id,dir_index,action,reason,available_blocks,status,details_json)
		 VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, d := range strips {
		if _, err := stmt.ExecContext(ctx, now, d.StripeID, d.ObjectID, d.DirIndex, d.Action,
			d.Reason, d.AvailableBlocks, d.Status, d.Details); err != nil {
			return err
		}
	}
	if err := insertAuditTx(ctx, tx, "block_dir", action, "", "", dirIndex, reason, details); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) RecordDegradedRead(ctx context.Context, stripeID, objectID, reason, details string) error {
	return insertAudit(ctx, s.db, "read", "degraded", stripeID, objectID, -1, reason, details)
}

func (s *SQLite) RecordBlockMismatch(ctx context.Context, stripeID, objectID string, dirIndex int, reason, details string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertAuditTx(ctx, tx, "block", "invalid", stripeID, objectID, dirIndex, reason, details); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO degradations(ts,stripe_id,object_id,dir_index,action,reason,available_blocks,status,details_json)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		nowRFC3339(), stripeID, objectID, dirIndex, "block_invalid", reason, 0, "degraded", details); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) RecordRepairSkip(ctx context.Context, stripeID, objectID, reason, details string) error {
	return insertAudit(ctx, s.db, "repair", "skipped", stripeID, objectID, -1, reason, details)
}

func insertAudit(ctx context.Context, db *sql.DB, action, status, stripeID, objectID string, dirIndex int, reason, details string) error {
	if details == "" {
		details = "{}"
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO audit_events(ts,action,status,stripe_id,object_id,dir_index,reason,details_json)
		 VALUES(?,?,?,?,?,?,?,?)`,
		nowRFC3339(), action, status, stripeID, objectID, dirIndex, reason, details)
	return err
}

func insertAuditTx(ctx context.Context, tx *sql.Tx, action, status, stripeID, objectID string, dirIndex int, reason, details string) error {
	if details == "" {
		details = "{}"
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit_events(ts,action,status,stripe_id,object_id,dir_index,reason,details_json)
		 VALUES(?,?,?,?,?,?,?,?)`,
		nowRFC3339(), action, status, stripeID, objectID, dirIndex, reason, details)
	return err
}

type AuditEvent struct {
	ID        int64  `json:"id"`
	Timestamp string `json:"ts"`
	Action    string `json:"action"`
	Status    string `json:"status"`
	StripeID  string `json:"stripe_id,omitempty"`
	ObjectID  string `json:"object_id,omitempty"`
	DirIndex  int    `json:"dir_index,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Details   any    `json:"details"`
}

type DegradationRow struct {
	ID              int64  `json:"id"`
	Timestamp       string `json:"ts"`
	StripeID        string `json:"stripe_id"`
	ObjectID        string `json:"object_id"`
	DirIndex        int    `json:"dir_index"`
	Action          string `json:"action"`
	Reason          string `json:"reason"`
	AvailableBlocks int    `json:"available_blocks"`
	Status          string `json:"status"`
	Details         any    `json:"details"`
}

type RepairRow struct {
	JobID         string `json:"job_id"`
	StripeID      string `json:"stripe_id"`
	ObjectID      string `json:"object_id"`
	MissingBlocks []int  `json:"missing_blocks"`
	State         string `json:"state"`
	ReservedBytes int64  `json:"reserved_bytes"`
	Day           string `json:"day"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
	Error         string `json:"error,omitempty"`
	Result        any    `json:"result"`
}

type QuotaRow struct {
	Day       string `json:"day"`
	JobID     string `json:"job_id"`
	StripeID  string `json:"stripe_id"`
	Bytes     int64  `json:"bytes"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type AuditResult struct {
	StripeID     string           `json:"stripe_id"`
	ObjectID     string           `json:"object_id,omitempty"`
	Events       []AuditEvent     `json:"events"`
	Degradations []DegradationRow `json:"degradations"`
	Repairs      []RepairRow      `json:"repairs"`
	Quota        []QuotaRow       `json:"quota"`
}

func (s *SQLite) AuditForStripe(ctx context.Context, stripeID string) (AuditResult, error) {
	out := AuditResult{StripeID: stripeID, Events: []AuditEvent{}, Degradations: []DegradationRow{}, Repairs: []RepairRow{}, Quota: []QuotaRow{}}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,ts,action,status,stripe_id,object_id,dir_index,reason,details_json
		 FROM audit_events WHERE stripe_id=? ORDER BY id`, stripeID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e AuditEvent
		var details string
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Action, &e.Status, &e.StripeID, &e.ObjectID, &e.DirIndex, &e.Reason, &details); err != nil {
			rows.Close()
			return out, err
		}
		e.Details = decodeJSON(details)
		out.Events = append(out.Events, e)
		if out.ObjectID == "" {
			out.ObjectID = e.ObjectID
		}
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx,
		`SELECT id,ts,stripe_id,object_id,dir_index,action,reason,available_blocks,status,details_json
		 FROM degradations WHERE stripe_id=? ORDER BY id`, stripeID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var d DegradationRow
		var details string
		if err := rows.Scan(&d.ID, &d.Timestamp, &d.StripeID, &d.ObjectID, &d.DirIndex, &d.Action,
			&d.Reason, &d.AvailableBlocks, &d.Status, &details); err != nil {
			rows.Close()
			return out, err
		}
		d.Details = decodeJSON(details)
		out.Degradations = append(out.Degradations, d)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx,
		`SELECT job_id,stripe_id,object_id,missing_blocks,state,reserved_bytes,day,started_at,finished_at,error,result_json
		 FROM repair_jobs WHERE stripe_id=? ORDER BY started_at, job_id`, stripeID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r RepairRow
		var missing, result string
		if err := rows.Scan(&r.JobID, &r.StripeID, &r.ObjectID, &missing, &r.State, &r.ReservedBytes,
			&r.Day, &r.StartedAt, &r.FinishedAt, &r.Error, &result); err != nil {
			rows.Close()
			return out, err
		}
		_ = json.Unmarshal([]byte(missing), &r.MissingBlocks)
		r.Result = decodeJSON(result)
		out.Repairs = append(out.Repairs, r)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx,
		`SELECT day,job_id,stripe_id,bytes,state,created_at,updated_at
		 FROM quota_ledger WHERE stripe_id=? ORDER BY id`, stripeID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var q QuotaRow
		if err := rows.Scan(&q.Day, &q.JobID, &q.StripeID, &q.Bytes, &q.State, &q.CreatedAt, &q.UpdatedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.Quota = append(out.Quota, q)
	}
	rows.Close()
	return out, rows.Err()
}

func decodeJSON(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	if v == nil {
		return map[string]any{}
	}
	return v
}

type ReconciliationRow struct {
	ID                  string `json:"id"`
	StartedAt           string `json:"started_at"`
	FinishedAt          string `json:"finished_at,omitempty"`
	State               string `json:"state"`
	MissingCount        int    `json:"missing_count"`
	LengthMismatchCount int    `json:"length_mismatch_count"`
	DigestMismatchCount int    `json:"digest_mismatch_count"`
	Result              any    `json:"result"`
	Error               string `json:"error,omitempty"`
}

func (s *SQLite) StartReconciliation(ctx context.Context, id, startedAt, details string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO reconciliations(id,started_at,state,result_json) VALUES(?,?,?,?)`,
		id, startedAt, "running", "{}"); err != nil {
		return err
	}
	if err := insertAuditTx(ctx, tx, "reconcile", "running", "", "", -1, "snapshot_started", details); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) FinishReconciliation(ctx context.Context, id, state string, missing, length, digest int, resultJSON, errText, details string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE reconciliations SET finished_at=?, state=?, missing_count=?, length_mismatch_count=?,
		 digest_mismatch_count=?, result_json=?, error=? WHERE id=?`,
		nowRFC3339(), state, missing, length, digest, resultJSON, errText, id); err != nil {
		return err
	}
	if err := insertAuditTx(ctx, tx, "reconcile", state, "", "", -1, "reconcile_finished", details); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLite) Reconciliation(ctx context.Context, id string) (ReconciliationRow, error) {
	var r ReconciliationRow
	var result string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,started_at,finished_at,state,missing_count,length_mismatch_count,digest_mismatch_count,result_json,error
		 FROM reconciliations WHERE id=?`, id).Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.State,
		&r.MissingCount, &r.LengthMismatchCount, &r.DigestMismatchCount, &result, &r.Error)
	if err != nil {
		return r, err
	}
	r.Result = decodeJSON(result)
	return r, nil
}

func (s *SQLite) LastReconciliation(ctx context.Context) (ReconciliationRow, error) {
	var r ReconciliationRow
	var result string
	err := s.db.QueryRowContext(ctx,
		`SELECT id,started_at,finished_at,state,missing_count,length_mismatch_count,digest_mismatch_count,result_json,error
		 FROM reconciliations ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&r.ID, &r.StartedAt, &r.FinishedAt,
		&r.State, &r.MissingCount, &r.LengthMismatchCount, &r.DigestMismatchCount, &result, &r.Error)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	r.Result = decodeJSON(result)
	return r, nil
}

var ErrNotFound = errors.New("not found")

func NormalizeMissing(raw string) string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}
