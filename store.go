package dressbatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	now func() time.Time
}

type RunRecord struct {
	RunID             string `json:"run_id"`
	ProjectID         string `json:"project_id"`
	ProjectHash       string `json:"project_hash"`
	ConstraintVersion string `json:"constraint_version"`
	AlgorithmVersion  string `json:"algorithm_version"`
	Status            string `json:"status"`
	CreatedAt         string `json:"created_at"`
}

type BatchCheckpoint struct {
	RunID             string
	CommitSeq         int
	ProjectHash       string
	ConstraintVersion string
	AlgorithmVersion  string
	MaterialVersion   string
	Batch             Batch
}

type RecoveryReport struct {
	RunID             string   `json:"run_id"`
	RunStatus         string   `json:"run_status"`
	ValidPrefix       int      `json:"valid_prefix"`
	NextCommitSeq     int      `json:"next_commit_seq"`
	ValidBatches      []Batch  `json:"-"`
	ValidBatchIDs     []string `json:"valid_batch_ids"`
	DiscardedBatchIDs []string `json:"discarded_batch_ids"`
	DiscardedFrom     int      `json:"discarded_from,omitempty"`
	Truncated         bool     `json:"truncated"`
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, newError(CodeStore, "store", "path", "database path is required")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		parent := filepath.Dir(path)
		if parent != "." {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return nil, newError(CodeStore, "store", "path", "cannot create database directory: %v", err)
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, newError(CodeStore, "store", "open", "cannot open sqlite database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := store.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) initialize() error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`CREATE TABLE IF NOT EXISTS runs (
			run_id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			project_hash TEXT NOT NULL,
			constraint_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS batch_checkpoints (
			run_id TEXT NOT NULL,
			commit_seq INTEGER NOT NULL,
			batch_id TEXT NOT NULL,
			batch_index INTEGER NOT NULL,
			project_hash TEXT NOT NULL,
			batch_content_hash TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			constraint_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			material_version TEXT NOT NULL,
			dependencies_json TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			committed_at TEXT NOT NULL,
			PRIMARY KEY (run_id, commit_seq),
			UNIQUE (run_id, batch_id),
			FOREIGN KEY (run_id) REFERENCES runs(run_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_batch_checkpoints_run_index ON batch_checkpoints(run_id, batch_index)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return newError(CodeStore, "store", "schema", "cannot initialize sqlite store: %v", err)
		}
	}
	return nil
}

func NewRunRecord(runID string, project *Project) RunRecord {
	return RunRecord{
		RunID:             runID,
		ProjectID:         project.ID,
		ProjectHash:       ProjectContentHash(project),
		ConstraintVersion: ConstraintVersion(project.Constraints),
		AlgorithmVersion:  AlgorithmVersion,
		Status:            "open",
	}
}

func (s *Store) BeginRun(ctx context.Context, run RunRecord) error {
	if run.RunID == "" {
		return newError(CodeStore, "run", "run_id", "field is required")
	}
	if run.ProjectID == "" {
		return newError(CodeStore, "run", "project_id", "field is required")
	}
	if run.ProjectHash == "" {
		return newError(CodeStore, "run", "project_hash", "field is required")
	}
	if run.ConstraintVersion == "" {
		return newError(CodeStore, "run", "constraint_version", "field is required")
	}
	if run.AlgorithmVersion == "" {
		return newError(CodeStore, "run", "algorithm_version", "field is required")
	}
	if run.Status == "" {
		run.Status = "open"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs(run_id, project_id, project_hash, constraint_version, algorithm_version, status, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO NOTHING`,
		run.RunID, run.ProjectID, run.ProjectHash, run.ConstraintVersion, run.AlgorithmVersion, run.Status, now,
	)
	if err != nil {
		return newError(CodeStore, "run:"+run.RunID, "insert", "cannot create run: %v", err)
	}
	existing, err := s.loadRunLocked(ctx, run.RunID)
	if err != nil {
		return err
	}
	if existing.ProjectID != run.ProjectID ||
		existing.ProjectHash != run.ProjectHash ||
		existing.ConstraintVersion != run.ConstraintVersion ||
		existing.AlgorithmVersion != run.AlgorithmVersion {
		return withDetails(
			newError(CodeStore, "run:"+run.RunID, "metadata", "run metadata does not match the existing run"),
			map[string]any{"existing": existing, "requested": run},
		)
	}
	return nil
}

func (s *Store) AppendBatch(ctx context.Context, checkpoint BatchCheckpoint) error {
	if checkpoint.RunID == "" {
		return newError(CodeStore, "checkpoint", "run_id", "field is required")
	}
	if checkpoint.CommitSeq <= 0 {
		return newError(CodeStore, "checkpoint", "commit_seq", "must be greater than zero")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return newError(CodeStore, "checkpoint", "transaction", "cannot begin checkpoint transaction: %v", err)
	}
	defer tx.Rollback()
	if err := s.appendBatchTx(ctx, tx, checkpoint); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return newError(CodeStore, "checkpoint", "commit", "cannot commit checkpoint: %v", err)
	}
	return nil
}

func (s *Store) AppendNextBatch(ctx context.Context, runID string, project *Project, batch Batch) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.loadRunLocked(ctx, runID)
	if err != nil {
		return 0, err
	}
	if run.ProjectHash != ProjectContentHash(project) {
		return 0, newError(CodeRecovery, "run:"+runID, "project_hash", "project content hash does not match run")
	}
	var maxSeq int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(commit_seq), 0) FROM batch_checkpoints WHERE run_id = ?`, runID).Scan(&maxSeq); err != nil {
		return 0, newError(CodeStore, "run:"+runID, "commit_seq", "cannot read last commit sequence: %v", err)
	}
	checkpoint := BatchCheckpoint{
		RunID:             runID,
		CommitSeq:         maxSeq + 1,
		ProjectHash:       run.ProjectHash,
		ConstraintVersion: run.ConstraintVersion,
		AlgorithmVersion:  run.AlgorithmVersion,
		MaterialVersion:   MaterialVersion(project, batch.MaterialSlots),
		Batch:             batch,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, newError(CodeStore, "checkpoint", "transaction", "cannot begin checkpoint transaction: %v", err)
	}
	defer tx.Rollback()
	if err := s.appendBatchTx(ctx, tx, checkpoint); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, newError(CodeStore, "checkpoint", "commit", "cannot commit checkpoint: %v", err)
	}
	return checkpoint.CommitSeq, nil
}

func (s *Store) appendBatchTx(ctx context.Context, tx *sql.Tx, checkpoint BatchCheckpoint) error {
	run, err := loadRunTx(ctx, tx, checkpoint.RunID)
	if err != nil {
		return err
	}
	if checkpoint.ProjectHash != "" && checkpoint.ProjectHash != run.ProjectHash {
		return newError(CodeStore, "run:"+checkpoint.RunID, "project_hash", "checkpoint project hash does not match run")
	}
	if checkpoint.ConstraintVersion != "" && checkpoint.ConstraintVersion != run.ConstraintVersion {
		return newError(CodeStore, "batch:"+checkpoint.Batch.ID, "constraint_version", "checkpoint constraint version does not match run")
	}
	if checkpoint.AlgorithmVersion != "" && checkpoint.AlgorithmVersion != run.AlgorithmVersion {
		return newError(CodeStore, "batch:"+checkpoint.Batch.ID, "algorithm_version", "checkpoint algorithm version does not match run")
	}
	if checkpoint.Batch.Index != checkpoint.CommitSeq {
		return newError(CodeStore, "batch:"+checkpoint.Batch.ID, "batch_index", "batch index must equal commit sequence")
	}
	var lastSeq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(commit_seq), 0) FROM batch_checkpoints WHERE run_id = ?`, checkpoint.RunID).Scan(&lastSeq); err != nil {
		return newError(CodeStore, "run:"+checkpoint.RunID, "commit_seq", "cannot read last commit sequence: %v", err)
	}
	if checkpoint.CommitSeq != lastSeq+1 {
		return withDetails(
			newError(CodeStore, "batch:"+checkpoint.Batch.ID, "commit_seq", "checkpoint commit sequence is not contiguous"),
			map[string]any{"last_commit_seq": lastSeq, "requested_commit_seq": checkpoint.CommitSeq},
		)
	}
	contentHash := BatchContentHash(checkpoint.Batch)
	if checkpoint.Batch.ContentHash != "" && checkpoint.Batch.ContentHash != contentHash {
		return newError(CodeStore, "batch:"+checkpoint.Batch.ID, "content_hash", "batch content hash is invalid")
	}
	checkpoint.Batch.ContentHash = contentHash
	if checkpoint.Batch.CacheKey == "" {
		return newError(CodeStore, "batch:"+checkpoint.Batch.ID, "cache_key", "field is required")
	}
	payload, err := json.Marshal(checkpoint.Batch)
	if err != nil {
		return newError(CodeStore, "batch:"+checkpoint.Batch.ID, "payload_json", "cannot encode batch: %v", err)
	}
	dependencies, err := json.Marshal(sortedUnique(checkpoint.Batch.Dependencies))
	if err != nil {
		return newError(CodeStore, "batch:"+checkpoint.Batch.ID, "dependencies", "cannot encode dependencies: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO batch_checkpoints(
			run_id, commit_seq, batch_id, batch_index, project_hash, batch_content_hash,
			cache_key, constraint_version, algorithm_version, material_version,
			dependencies_json, payload_json, committed_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		checkpoint.RunID,
		checkpoint.CommitSeq,
		checkpoint.Batch.ID,
		checkpoint.Batch.Index,
		run.ProjectHash,
		contentHash,
		checkpoint.Batch.CacheKey,
		run.ConstraintVersion,
		run.AlgorithmVersion,
		checkpoint.MaterialVersion,
		string(dependencies),
		string(payload),
		s.now().Format(time.RFC3339Nano),
	)
	if err != nil {
		return newError(CodeStore, "batch:"+checkpoint.Batch.ID, "insert", "cannot append checkpoint: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = 'open' WHERE run_id = ?`, checkpoint.RunID); err != nil {
		return newError(CodeStore, "run:"+checkpoint.RunID, "status", "cannot update run status: %v", err)
	}
	return nil
}

// ReplacePlan atomically replaces all committed batches in a run. It is used by
// the incremental path after the replacement has been verified against a full
// repack. Normal batch commits should use AppendBatch / AppendNextBatch.
func (s *Store) ReplacePlan(ctx context.Context, run RunRecord, project *Project, batches []Batch) error {
	if run.RunID == "" {
		return newError(CodeStore, "run", "run_id", "field is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return newError(CodeStore, "checkpoint", "transaction", "cannot begin replacement transaction: %v", err)
	}
	defer tx.Rollback()
	if err := s.ensureRunTx(ctx, tx, run); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM batch_checkpoints WHERE run_id = ?`, run.RunID); err != nil {
		return newError(CodeStore, "run:"+run.RunID, "checkpoints", "cannot clear existing checkpoints: %v", err)
	}
	upstreamKey := ""
	for i, batch := range batches {
		if batch.Index != i+1 {
			return newError(CodeStore, "batch:"+batch.ID, "batch_index", "batches must be supplied in contiguous commit order")
		}
		batch.CacheKey = BatchCacheKey(project, batch, upstreamKey)
		batch.ContentHash = BatchContentHash(batch)
		upstreamKey = batch.CacheKey
		checkpoint := BatchCheckpoint{
			RunID:             run.RunID,
			CommitSeq:         i + 1,
			ProjectHash:       run.ProjectHash,
			ConstraintVersion: run.ConstraintVersion,
			AlgorithmVersion:  run.AlgorithmVersion,
			MaterialVersion:   MaterialVersion(project, batch.MaterialSlots),
			Batch:             batch,
		}
		if err := s.appendBatchTx(ctx, tx, checkpoint); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = 'committed' WHERE run_id = ?`, run.RunID); err != nil {
		return newError(CodeStore, "run:"+run.RunID, "status", "cannot mark run committed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return newError(CodeStore, "checkpoint", "commit", "cannot commit replacement: %v", err)
	}
	return nil
}

func (s *Store) ensureRunTx(ctx context.Context, tx *sql.Tx, run RunRecord) error {
	if run.Status == "" {
		run.Status = "open"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO runs(run_id, project_id, project_hash, constraint_version, algorithm_version, status, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO NOTHING`,
		run.RunID, run.ProjectID, run.ProjectHash, run.ConstraintVersion, run.AlgorithmVersion, run.Status, s.now().Format(time.RFC3339Nano),
	)
	if err != nil {
		return newError(CodeStore, "run:"+run.RunID, "insert", "cannot create run: %v", err)
	}
	existing, err := loadRunTx(ctx, tx, run.RunID)
	if err != nil {
		return err
	}
	if existing.ProjectID != run.ProjectID ||
		existing.ProjectHash != run.ProjectHash ||
		existing.ConstraintVersion != run.ConstraintVersion ||
		existing.AlgorithmVersion != run.AlgorithmVersion {
		return newError(CodeStore, "run:"+run.RunID, "metadata", "run metadata does not match existing run")
	}
	return nil
}

func (s *Store) loadRunLocked(ctx context.Context, runID string) (RunRecord, error) {
	var run RunRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT run_id, project_id, project_hash, constraint_version, algorithm_version, status, created_at
		FROM runs WHERE run_id = ?`, runID,
	).Scan(&run.RunID, &run.ProjectID, &run.ProjectHash, &run.ConstraintVersion, &run.AlgorithmVersion, &run.Status, &run.CreatedAt)
	if err == sql.ErrNoRows {
		return RunRecord{}, newError(CodeRecovery, "run:"+runID, "run_id", "run does not exist")
	}
	if err != nil {
		return RunRecord{}, newError(CodeStore, "run:"+runID, "query", "cannot load run: %v", err)
	}
	return run, nil
}

func loadRunTx(ctx context.Context, tx *sql.Tx, runID string) (RunRecord, error) {
	var run RunRecord
	err := tx.QueryRowContext(ctx, `
		SELECT run_id, project_id, project_hash, constraint_version, algorithm_version, status, created_at
		FROM runs WHERE run_id = ?`, runID,
	).Scan(&run.RunID, &run.ProjectID, &run.ProjectHash, &run.ConstraintVersion, &run.AlgorithmVersion, &run.Status, &run.CreatedAt)
	if err == sql.ErrNoRows {
		return RunRecord{}, newError(CodeRecovery, "run:"+runID, "run_id", "run does not exist")
	}
	if err != nil {
		return RunRecord{}, newError(CodeStore, "run:"+runID, "query", "cannot load run: %v", err)
	}
	return run, nil
}

type storedCheckpoint struct {
	CommitSeq         int
	BatchID           string
	BatchIndex        int
	ProjectHash       string
	BatchContentHash  string
	CacheKey          string
	ConstraintVersion string
	AlgorithmVersion  string
	MaterialVersion   string
	DependenciesJSON  string
	PayloadJSON       string
	CommittedAt       string
}

func (s *Store) RecoverRun(ctx context.Context, runID string, project *Project) (*RecoveryReport, error) {
	if project == nil || project.ID == "" {
		return nil, newError(CodeRecovery, "run:"+runID, "project", "project is required for recovery")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.loadRunLocked(ctx, runID)
	if err != nil {
		return nil, err
	}
	report := &RecoveryReport{
		RunID:             runID,
		RunStatus:         run.Status,
		NextCommitSeq:     1,
		ValidBatches:      []Batch{},
		ValidBatchIDs:     []string{},
		DiscardedBatchIDs: []string{},
	}
	runMetadataValid := run.ProjectID == project.ID &&
		run.ProjectHash == ProjectContentHash(project) &&
		run.ConstraintVersion == ConstraintVersion(project.Constraints) &&
		run.AlgorithmVersion == AlgorithmVersion
	if !runMetadataValid {
		rows, _ := s.loadStoredCheckpoints(ctx, runID)
		for _, row := range rows {
			report.DiscardedBatchIDs = append(report.DiscardedBatchIDs, row.BatchID)
		}
		if len(rows) > 0 {
			report.DiscardedFrom = rows[0].CommitSeq
			report.Truncated = true
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM batch_checkpoints WHERE run_id = ?`, runID); err != nil {
			return nil, newError(CodeRecovery, "run:"+runID, "truncate", "cannot discard incompatible run: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET status = 'invalid' WHERE run_id = ?`, runID); err != nil {
			return nil, newError(CodeRecovery, "run:"+runID, "status", "cannot mark run invalid: %v", err)
		}
		report.RunStatus = "invalid"
		return report, nil
	}
	rows, err := s.loadStoredCheckpoints(ctx, runID)
	if err != nil {
		return nil, err
	}
	upstreamKey := ""
	expectedSeq := 1
	discardFrom := 0
	for _, row := range rows {
		if discardFrom != 0 {
			report.DiscardedBatchIDs = append(report.DiscardedBatchIDs, row.BatchID)
			continue
		}
		if row.CommitSeq != expectedSeq || row.BatchIndex != expectedSeq {
			discardFrom = row.CommitSeq
			report.DiscardedBatchIDs = append(report.DiscardedBatchIDs, row.BatchID)
			continue
		}
		var batch Batch
		if err := json.Unmarshal([]byte(row.PayloadJSON), &batch); err != nil {
			discardFrom = row.CommitSeq
			report.DiscardedBatchIDs = append(report.DiscardedBatchIDs, row.BatchID)
			continue
		}
		invalid := row.ProjectHash != run.ProjectHash ||
			row.ConstraintVersion != run.ConstraintVersion ||
			row.AlgorithmVersion != run.AlgorithmVersion ||
			row.MaterialVersion != MaterialVersion(project, batch.MaterialSlots) ||
			row.BatchContentHash != BatchContentHash(batch) ||
			batch.ContentHash != row.BatchContentHash ||
			row.CacheKey != batch.CacheKey ||
			row.CacheKey != BatchCacheKey(project, batch, upstreamKey) ||
			row.DependenciesJSON != mustJSON(sortedUnique(batch.Dependencies))
		if invalid {
			discardFrom = row.CommitSeq
			report.DiscardedBatchIDs = append(report.DiscardedBatchIDs, row.BatchID)
			continue
		}
		report.ValidBatches = append(report.ValidBatches, batch)
		report.ValidBatchIDs = append(report.ValidBatchIDs, batch.ID)
		upstreamKey = row.CacheKey
		expectedSeq++
	}
	if discardFrom != 0 {
		report.Truncated = true
		report.DiscardedFrom = discardFrom
		if _, err := s.db.ExecContext(ctx, `DELETE FROM batch_checkpoints WHERE run_id = ? AND commit_seq >= ?`, runID, discardFrom); err != nil {
			return nil, newError(CodeRecovery, "run:"+runID, "truncate", "cannot discard invalid checkpoint suffix: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET status = 'truncated' WHERE run_id = ?`, runID); err != nil {
			return nil, newError(CodeRecovery, "run:"+runID, "status", "cannot mark run truncated: %v", err)
		}
		report.RunStatus = "truncated"
	}
	report.ValidPrefix = len(report.ValidBatches)
	report.NextCommitSeq = report.ValidPrefix + 1
	return report, nil
}

func (s *Store) loadStoredCheckpoints(ctx context.Context, runID string) ([]storedCheckpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT commit_seq, batch_id, batch_index, project_hash, batch_content_hash,
		       cache_key, constraint_version, algorithm_version, material_version,
		       dependencies_json, payload_json, committed_at
		FROM batch_checkpoints WHERE run_id = ? ORDER BY commit_seq ASC`, runID)
	if err != nil {
		return nil, newError(CodeRecovery, "run:"+runID, "checkpoints", "cannot query checkpoints: %v", err)
	}
	defer rows.Close()
	out := make([]storedCheckpoint, 0)
	for rows.Next() {
		var row storedCheckpoint
		if err := rows.Scan(
			&row.CommitSeq, &row.BatchID, &row.BatchIndex, &row.ProjectHash, &row.BatchContentHash,
			&row.CacheKey, &row.ConstraintVersion, &row.AlgorithmVersion, &row.MaterialVersion,
			&row.DependenciesJSON, &row.PayloadJSON, &row.CommittedAt,
		); err != nil {
			return nil, newError(CodeRecovery, "run:"+runID, "checkpoints", "cannot scan checkpoint: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, newError(CodeRecovery, "run:"+runID, "checkpoints", "cannot iterate checkpoints: %v", err)
	}
	return out, nil
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func (s *Store) ListCheckpointIDs(ctx context.Context, runID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.loadStoredCheckpoints(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.BatchID)
	}
	return out, nil
}

func (s *Store) CommitResult(ctx context.Context, run RunRecord, project *Project, result *PackResult) error {
	if err := s.BeginRun(ctx, run); err != nil {
		return err
	}
	for _, batch := range result.Batches {
		if err := s.AppendBatch(ctx, BatchCheckpoint{
			RunID:             run.RunID,
			CommitSeq:         batch.Index,
			ProjectHash:       run.ProjectHash,
			ConstraintVersion: run.ConstraintVersion,
			AlgorithmVersion:  run.AlgorithmVersion,
			MaterialVersion:   MaterialVersion(project, batch.MaterialSlots),
			Batch:             batch,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CountCheckpoints(ctx context.Context, runID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM batch_checkpoints WHERE run_id = ?`, runID).Scan(&count); err != nil {
		return 0, newError(CodeStore, "run:"+runID, "checkpoints", "cannot count checkpoints: %v", err)
	}
	return count, nil
}

func (s *Store) CheckpointSequence(ctx context.Context, runID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT batch_id FROM batch_checkpoints WHERE run_id = ? ORDER BY commit_seq ASC`, runID)
	if err != nil {
		return nil, newError(CodeStore, "run:"+runID, "checkpoints", "cannot query sequence: %v", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var batchID string
		if err := rows.Scan(&batchID); err != nil {
			return nil, newError(CodeStore, "run:"+runID, "checkpoints", "cannot scan sequence: %v", err)
		}
		out = append(out, batchID)
	}
	return out, rows.Err()
}

func (s *Store) RunRecord(ctx context.Context, runID string) (RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadRunLocked(ctx, runID)
}

func (s *Store) TamperForTest(ctx context.Context, runID string, commitSeq int, column string, value string) error {
	allowed := map[string]bool{
		"batch_content_hash": true,
		"cache_key":          true,
		"material_version":   true,
		"constraint_version": true,
		"algorithm_version":  true,
		"payload_json":       true,
	}
	if !allowed[column] {
		return fmt.Errorf("unsupported tamper column %q", column)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	query := fmt.Sprintf("UPDATE batch_checkpoints SET %s = ? WHERE run_id = ? AND commit_seq = ?", column)
	result, err := s.db.ExecContext(ctx, query, value, runID, commitSeq)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("checkpoint not found")
	}
	return nil
}

func sortedCheckpointIDs(checkpoints []BatchCheckpoint) []string {
	out := make([]string, 0, len(checkpoints))
	sort.Slice(checkpoints, func(i, j int) bool { return checkpoints[i].CommitSeq < checkpoints[j].CommitSeq })
	for _, checkpoint := range checkpoints {
		out = append(out, checkpoint.Batch.ID)
	}
	return out
}
