package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"settlement/internal/tasks"
)

type Store struct {
	db *sql.DB
}

type CommittedResult struct {
	BatchID         string
	HolderID        string
	TaskCount       int32
	TotalAmount     int64
	ResultDigest    string
	ResultJSON      string
	CommittedAtUnix int64
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 200; attempt++ {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		lastErr = initialize(db)
		if lastErr == nil {
			return &Store{db: db}, nil
		}
		_ = db.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("initialize sqlite after retries: %w", lastErr)
}

func initialize(db *sql.DB) error {
	for _, pragma := range []string{
		"PRAGMA busy_timeout = 30000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return fmt.Errorf("%s: %w", pragma, err)
		}
	}
	store := &Store{db: db}
	return store.migrate()
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS settlements (
    batch_id TEXT PRIMARY KEY,
    holder_id TEXT NOT NULL,
    task_count INTEGER NOT NULL,
    total_amount INTEGER NOT NULL,
    result_digest TEXT NOT NULL,
    result_json TEXT NOT NULL,
    committed_at_unix_ms INTEGER NOT NULL,
    persisted_at_unix_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settlement_tasks (
    batch_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    amount INTEGER NOT NULL,
    PRIMARY KEY (batch_id, task_id)
);
CREATE INDEX IF NOT EXISTS idx_settlement_tasks_batch ON settlement_tasks(batch_id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return nil
}

func (s *Store) Apply(ctx context.Context, result CommittedResult) error {
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		lastErr = s.applyOnce(ctx, result)
		if lastErr == nil {
			return nil
		}
		if !isBusyError(lastErr) || time.Now().After(deadline) {
			return lastErr
		}
		timer := time.NewTimer(30 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) applyOnce(ctx context.Context, result CommittedResult) error {
	rows, err := tasks.ParseResult(result.ResultJSON)
	if err != nil {
		return fmt.Errorf("parse canonical result: %w", err)
	}
	if int32(len(rows)) != result.TaskCount {
		return fmt.Errorf("result task count mismatch: payload=%d declared=%d", len(rows), result.TaskCount)
	}
	sum := sha256.Sum256([]byte("settlement-v1\n" + result.BatchID + "\n" + result.ResultJSON))
	if hex.EncodeToString(sum[:]) != result.ResultDigest {
		return errors.New("canonical result digest mismatch")
	}
	var total int64
	for _, row := range rows {
		total += row.Amount
	}
	if total != result.TotalAmount {
		return fmt.Errorf("result total mismatch: payload=%d declared=%d", total, result.TotalAmount)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var existingDigest, existingPayload string
	err = tx.QueryRowContext(ctx,
		`SELECT result_digest, result_json FROM settlements WHERE batch_id = ?`,
		result.BatchID,
	).Scan(&existingDigest, &existingPayload)
	switch {
	case err == nil:
		if existingDigest != result.ResultDigest || existingPayload != result.ResultJSON {
			return fmt.Errorf("batch %s already persisted with a different result", result.BatchID)
		}
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM settlement_tasks WHERE batch_id = ?`,
			result.BatchID,
		).Scan(&count); err != nil {
			return err
		}
		if count != int(result.TaskCount) {
			return fmt.Errorf("batch %s has %d persisted task rows, expected %d", result.BatchID, count, result.TaskCount)
		}
		return tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO settlements (
    batch_id, holder_id, task_count, total_amount, result_digest,
    result_json, committed_at_unix_ms, persisted_at_unix_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		result.BatchID, result.HolderID, result.TaskCount, result.TotalAmount,
		result.ResultDigest, result.ResultJSON, result.CommittedAtUnix, now,
	); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO settlement_tasks (batch_id, task_id, amount) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range rows {
		if _, err := stmt.ExecContext(ctx, result.BatchID, row.TaskID, row.Amount); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "busy")
}

type BatchSummary struct {
	BatchID         string `json:"batch_id"`
	HolderID        string `json:"holder_id"`
	TaskCount       int32  `json:"task_count"`
	TotalAmount     int64  `json:"total_amount"`
	ResultDigest    string `json:"result_digest"`
	CommittedAtUnix int64  `json:"committed_at_unix_ms"`
}

func (s *Store) Summaries(ctx context.Context) ([]BatchSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT batch_id, holder_id, task_count, total_amount, result_digest, committed_at_unix_ms
FROM settlements ORDER BY batch_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var summaries []BatchSummary
	for rows.Next() {
		var item BatchSummary
		if err := rows.Scan(&item.BatchID, &item.HolderID, &item.TaskCount, &item.TotalAmount,
			&item.ResultDigest, &item.CommittedAtUnix); err != nil {
			return nil, err
		}
		summaries = append(summaries, item)
	}
	return summaries, rows.Err()
}

func (s *Store) TaskCount(ctx context.Context, batchID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settlement_tasks WHERE batch_id = ?`, batchID).Scan(&count)
	return count, err
}

func (s *Store) HasBatch(ctx context.Context, batchID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settlements WHERE batch_id = ?`, batchID).Scan(&count)
	return count > 0, err
}

func (s *Store) IntegrityCheck(ctx context.Context) (string, error) {
	var result string
	err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result)
	return result, err
}

func EncodeSummary(summaries []BatchSummary) ([]byte, error) {
	return json.MarshalIndent(summaries, "", "  ")
}
