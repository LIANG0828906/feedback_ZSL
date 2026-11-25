package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"alertkernel/engine"
	"alertkernel/errs"
)

type Store struct {
	db   *sql.DB
	path string
}

type Recovery struct {
	LastT       int                 `json:"last_t"`
	BasisSeq    int                 `json:"basis_seq"`
	RuleVersion string              `json:"rule_version"`
	States      []engine.State      `json:"states"`
	Times       []engine.TimeResult `json:"times,omitempty"`
	ResetReason string              `json:"reset_reason,omitempty"`
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, errs.Wrap(errs.EIO, "checkpoint.dir", err, "create checkpoint directory: %v", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, classifySQLiteError(path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, classifySQLiteError(path, err)
	}
	store := &Store{db: db, path: path}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.integrityCheck(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS cursor (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			last_t INTEGER NOT NULL,
			basis_seq INTEGER NOT NULL,
			rule_version TEXT NOT NULL,
			committed_at_logical_ms INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS instance_state (
			rule_id TEXT NOT NULL,
			instance_key TEXT NOT NULL,
			t INTEGER NOT NULL,
			state TEXT NOT NULL,
			labels_json TEXT NOT NULL,
			state_json TEXT NOT NULL,
			PRIMARY KEY (rule_id, instance_key)
		)`,
		`CREATE TABLE IF NOT EXISTS adjudication_cache (
			t INTEGER NOT NULL,
			rule_id TEXT NOT NULL,
			instance_key TEXT NOT NULL,
			primary_reason TEXT NOT NULL,
			decision_json TEXT NOT NULL,
			PRIMARY KEY (t, rule_id, instance_key)
		)`,
		`CREATE TABLE IF NOT EXISTS time_result (
			t INTEGER PRIMARY KEY,
			result_json TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return classifySQLiteError(s.path, err)
		}
	}
	return nil
}

func (s *Store) integrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return classifySQLiteError(s.path, err)
	}
	if !strings.EqualFold(result, "ok") {
		return errs.New(errs.ECheckpointCorrupt, s.path, "sqlite integrity_check returned %q", result)
	}
	return nil
}

func (s *Store) SaveTime(ctx context.Context, result *engine.TimeResult, ruleVersion string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classifySQLiteError(s.path, err)
	}
	defer func() { _ = tx.Rollback() }()

	stateStmt, err := tx.PrepareContext(ctx, `INSERT INTO instance_state
		(rule_id, instance_key, t, state, labels_json, state_json)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(rule_id, instance_key) DO UPDATE SET
			t=excluded.t, state=excluded.state, labels_json=excluded.labels_json, state_json=excluded.state_json`)
	if err != nil {
		return classifySQLiteError(s.path, err)
	}
	defer stateStmt.Close()

	for _, state := range result.States {
		labelsJSON, err := json.Marshal(state.Labels)
		if err != nil {
			return errs.Wrap(errs.ECheckpoint, "instance_state.labels_json", err, "encode labels: %v", err)
		}
		stateJSON, err := json.Marshal(state)
		if err != nil {
			return errs.Wrap(errs.ECheckpoint, "instance_state.state_json", err, "encode state: %v", err)
		}
		if _, err := stateStmt.ExecContext(ctx, state.RuleID, state.InstanceKey, result.T, state.State, string(labelsJSON), string(stateJSON)); err != nil {
			return classifySQLiteError(s.path, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM adjudication_cache WHERE t = ?`, result.T); err != nil {
		return classifySQLiteError(s.path, err)
	}
	for _, decision := range result.SuppressionDetails {
		decisionJSON, err := json.Marshal(decision)
		if err != nil {
			return errs.Wrap(errs.ECheckpoint, "adjudication_cache.decision_json", err, "encode decision: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO adjudication_cache
			(t, rule_id, instance_key, primary_reason, decision_json)
			VALUES (?, ?, ?, ?, ?)`,
			result.T, decision.RuleID, decision.InstanceKey, decision.PrimaryReason, string(decisionJSON)); err != nil {
			return classifySQLiteError(s.path, err)
		}
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return errs.Wrap(errs.ECheckpoint, "time_result.result_json", err, "encode time result: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO time_result(t, result_json) VALUES (?, ?)
		ON CONFLICT(t) DO UPDATE SET result_json=excluded.result_json`, result.T, string(resultJSON)); err != nil {
		return classifySQLiteError(s.path, err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO cursor(id, last_t, basis_seq, rule_version, committed_at_logical_ms)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET last_t=excluded.last_t, basis_seq=excluded.basis_seq,
			rule_version=excluded.rule_version, committed_at_logical_ms=excluded.committed_at_logical_ms`,
		result.T, result.BasisSeqAfter, ruleVersion, result.AtMS); err != nil {
		return classifySQLiteError(s.path, err)
	}
	if err := tx.Commit(); err != nil {
		return classifySQLiteError(s.path, err)
	}
	return nil
}

func (s *Store) LoadRecovery(ctx context.Context, expectedRuleVersion string) (*Recovery, error) {
	recovery := &Recovery{LastT: -1}
	var lastT sql.NullInt64
	var basisSeq sql.NullInt64
	var version sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT last_t, basis_seq, rule_version FROM cursor WHERE id = 1`).
		Scan(&lastT, &basisSeq, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return recovery, nil
	}
	if err != nil {
		return nil, classifySQLiteError(s.path, err)
	}
	recovery.LastT = int(lastT.Int64)
	recovery.BasisSeq = int(basisSeq.Int64)
	recovery.RuleVersion = version.String
	if expectedRuleVersion != "" && recovery.RuleVersion != expectedRuleVersion {
		return nil, errs.New(errs.ECheckpointVersion, "cursor.rule_version",
			"checkpoint version %q does not match engineering version %q; full recompute required",
			recovery.RuleVersion, expectedRuleVersion)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT state_json FROM instance_state`)
	if err != nil {
		return nil, classifySQLiteError(s.path, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, classifySQLiteError(s.path, err)
		}
		var state engine.State
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return nil, errs.Wrap(errs.ECheckpointCorrupt, "instance_state.state_json", err, "decode state: %v", err)
		}
		recovery.States = append(recovery.States, state)
	}
	if err := rows.Err(); err != nil {
		return nil, classifySQLiteError(s.path, err)
	}

	timeRows, err := s.db.QueryContext(ctx, `SELECT result_json FROM time_result WHERE t <= ? ORDER BY t`, recovery.LastT)
	if err != nil {
		return nil, classifySQLiteError(s.path, err)
	}
	defer timeRows.Close()
	for timeRows.Next() {
		var raw string
		if err := timeRows.Scan(&raw); err != nil {
			return nil, classifySQLiteError(s.path, err)
		}
		var result engine.TimeResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, errs.Wrap(errs.ECheckpointCorrupt, "time_result.result_json", err, "decode result: %v", err)
		}
		recovery.Times = append(recovery.Times, result)
	}
	if err := timeRows.Err(); err != nil {
		return nil, classifySQLiteError(s.path, err)
	}
	return recovery, nil
}

func (s *Store) Reset(ctx context.Context) error {
	for _, statement := range []string{
		`DELETE FROM instance_state`,
		`DELETE FROM adjudication_cache`,
		`DELETE FROM time_result`,
		`DELETE FROM cursor`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return classifySQLiteError(s.path, err)
		}
	}
	return nil
}

func (s *Store) LoadMatchingOrReset(ctx context.Context, expectedRuleVersion string) (*Recovery, error) {
	recovery, err := s.LoadRecovery(ctx, expectedRuleVersion)
	if errs.IsCode(err, errs.ECheckpointVersion) {
		if resetErr := s.Reset(ctx); resetErr != nil {
			return nil, resetErr
		}
		return &Recovery{LastT: -1, ResetReason: errs.ECheckpointVersion}, nil
	}
	return recovery, err
}

func classifySQLiteError(path string, err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "not a database") || strings.Contains(message, "malformed") ||
		strings.Contains(message, "sqlite_notadb") || strings.Contains(message, "file is encrypted") {
		return errs.Wrap(errs.ECheckpointCorrupt, path, err, "corrupt or truncated checkpoint: %v", err)
	}
	return errs.Wrap(errs.ECheckpoint, path, err, "sqlite checkpoint operation failed: %v", err)
}

func TruncateForTest(path string, bytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return errs.Wrap(errs.EIO, path, err, "stat checkpoint: %v", err)
	}
	if bytes <= 0 || bytes >= info.Size() {
		return errs.New(errs.EValidation, "truncate.bytes", "bytes must be in (0,%d), got %d", info.Size(), bytes)
	}
	if err := os.Truncate(path, bytes); err != nil {
		return errs.Wrap(errs.EIO, path, err, "truncate checkpoint: %v", err)
	}
	return nil
}
