package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite"
)

type Checkpoint struct {
	db   *sql.DB
	path string
}

type CheckpointState struct {
	ProjectID          string         `json:"project_id"`
	ProjectVersion     string         `json:"project_version"`
	ProjectFingerprint string         `json:"project_fingerprint"`
	GlobalCommitSeq    int64          `json:"global_commit_seq"`
	LastExportBatchNo  int            `json:"last_export_batch_no"`
	Windows            []*Aggregate   `json:"windows"`
	Batches            []*ExportBatch `json:"batches"`
}

const sqliteHeader = "SQLite format 3\x00"

func OpenCheckpoint(path string) (*Checkpoint, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, NewError(CodeCheckpointOpen, "checkpoint.path", "cannot create directory: %v", err).WithContext("path", path)
		}
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		if info.Size() < 16 {
			return nil, NewError(CodeCheckpointCorrupt, "checkpoint.path", "truncated or empty checkpoint file").
				WithContext("path", path).WithContext("size", info.Size())
		}
		header := make([]byte, 16)
		file, err := os.Open(path)
		if err != nil {
			return nil, NewError(CodeCheckpointOpen, "checkpoint.path", "cannot open checkpoint: %v", err).WithContext("path", path)
		}
		n, _ := file.Read(header)
		_ = file.Close()
		if n < 16 || string(header) != sqliteHeader {
			return nil, NewError(CodeCheckpointCorrupt, "checkpoint.path", "truncated or invalid SQLite header").
				WithContext("path", path).WithContext("size", info.Size()).WithContext("header_bytes", n)
		}
	}

	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, NewError(CodeCheckpointOpen, "checkpoint.path", "cannot open SQLite: %v", err).WithContext("path", path)
	}
	check := &Checkpoint{db: db, path: path}
	if err := check.verifyIntegrity(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := check.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return check, nil
}

func (c *Checkpoint) Close() error { return c.db.Close() }

func (c *Checkpoint) verifyIntegrity() error {
	var result string
	err := c.db.QueryRow(`PRAGMA integrity_check`).Scan(&result)
	if err != nil {
		return NewError(CodeCheckpointCorrupt, "checkpoint.integrity_check", "checkpoint is unreadable or truncated: %v", err).
			WithContext("path", c.path)
	}
	if result != "ok" {
		return NewError(CodeCheckpointCorrupt, "checkpoint.integrity_check", "SQLite integrity check failed").
			WithContext("path", c.path).WithContext("result", result)
	}
	return nil
}

func (c *Checkpoint) initSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS window_states (
			window_key TEXT PRIMARY KEY,
			window_index INTEGER NOT NULL,
			level TEXT NOT NULL,
			service_id TEXT NOT NULL,
			route_id TEXT NOT NULL,
			status_code INTEGER NOT NULL,
			commit_seq INTEGER NOT NULL,
			payload_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS export_batches (
			batch_no INTEGER PRIMARY KEY,
			batch_id TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL,
			window_count INTEGER NOT NULL,
			record_count INTEGER NOT NULL,
			digest TEXT NOT NULL,
			payload_json TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return NewError(CodeCheckpointOpen, "checkpoint.schema", "cannot initialize schema: %v", err)
		}
	}
	return nil
}

func (c *Checkpoint) SaveProjectVersion(ctx context.Context, project *Project) error {
	return c.setMeta(ctx, "project_id", project.ProjectID)
}

func (c *Checkpoint) setMeta(ctx context.Context, key, value string) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO checkpoint_meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return NewError(CodeCheckpointWrite, "checkpoint.meta", "cannot write meta: %v", err).WithContext("key", key)
	}
	return nil
}

func (c *Checkpoint) meta(ctx context.Context, key string) (string, error) {
	var value string
	err := c.db.QueryRowContext(ctx, `SELECT value FROM checkpoint_meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", NewError(CodeCheckpointCorrupt, "checkpoint.meta", "cannot read meta: %v", err).WithContext("key", key)
	}
	return value, nil
}

// SaveWindowCommit persists all window rows first and only then advances the
// global commit sequence in the same SQLite transaction.
func (c *Checkpoint) SaveWindowCommit(ctx context.Context, project *Project, windows []*Aggregate) (int64, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, NewError(CodeCheckpointWrite, "checkpoint.tx", "cannot begin window transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM window_states`); err != nil {
		return 0, NewError(CodeCheckpointWrite, "window_states", "cannot replace window states: %v", err)
	}

	var currentSeq int64
	var seqText string
	_ = tx.QueryRowContext(ctx, `SELECT value FROM checkpoint_meta WHERE key='global_commit_seq'`).Scan(&seqText)
	if seqText != "" {
		_, _ = scanInt64(seqText, &currentSeq)
	}
	nextSeq := currentSeq + 1

	for _, agg := range windows {
		if agg == nil {
			continue
		}
		agg.Committed = true
		agg.CommitSeq = nextSeq
		payload, err := json.Marshal(agg)
		if err != nil {
			return 0, NewError(CodeCheckpointWrite, "window.payload", "cannot encode window: %v", err).WithContext("window_key", agg.Key.String())
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO window_states
			(window_key,window_index,level,service_id,route_id,status_code,commit_seq,payload_json)
			VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(window_key) DO UPDATE SET
				window_index=excluded.window_index, level=excluded.level, service_id=excluded.service_id,
				route_id=excluded.route_id, status_code=excluded.status_code, commit_seq=excluded.commit_seq,
				payload_json=excluded.payload_json`,
			agg.Key.String(), agg.Key.Index, agg.Key.Level, agg.Key.ServiceID, agg.Key.RouteID,
			agg.Key.StatusCode, nextSeq, string(payload))
		if err != nil {
			return 0, NewError(CodeCheckpointWrite, "window_states", "cannot write window state: %v", err).
				WithContext("window_key", agg.Key.String())
		}
	}

	metaWrites := []struct{ key, value string }{
		{"project_id", project.ProjectID},
		{"project_version", project.ProjectVersion},
		{"project_fingerprint", project.Fingerprint()},
		{"global_commit_seq", formatInt64(nextSeq)},
	}
	for _, meta := range metaWrites {
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_meta(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, meta.key, meta.value); err != nil {
			return 0, NewError(CodeCheckpointWrite, "checkpoint_meta.global_commit_seq", "cannot advance commit sequence: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, NewError(CodeCheckpointWrite, "checkpoint.commit", "cannot commit window transaction: %v", err)
	}
	return nextSeq, nil
}

func (c *Checkpoint) SaveExportBatch(ctx context.Context, batch *ExportBatch) error {
	if batch == nil {
		return NewError(CodeFieldRequired, "export_batch", "batch is required")
	}
	payload, err := json.Marshal(batch)
	if err != nil {
		return NewError(CodeCheckpointWrite, "export_batch.payload", "cannot encode batch: %v", err).WithContext("batch_id", batch.BatchID)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return NewError(CodeCheckpointWrite, "checkpoint.tx", "cannot begin export transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO export_batches
		(batch_no,batch_id,status,window_count,record_count,digest,payload_json)
		VALUES(?,?, 'committed', ?,?,?,?)
		ON CONFLICT(batch_no) DO UPDATE SET batch_id=excluded.batch_id, status='committed',
			window_count=excluded.window_count, record_count=excluded.record_count,
			digest=excluded.digest, payload_json=excluded.payload_json`,
		batch.BatchNo, batch.BatchID, batch.WindowCount, batch.RecordCount, batch.SnapshotDigest, string(payload))
	if err != nil {
		return NewError(CodeCheckpointWrite, "export_batches", "cannot write export batch: %v", err).WithContext("batch_id", batch.BatchID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_meta(key,value) VALUES('last_export_batch_no',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, formatInt(batch.BatchNo)); err != nil {
		return NewError(CodeCheckpointWrite, "checkpoint_meta.last_export_batch_no", "cannot update export sequence: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return NewError(CodeCheckpointWrite, "export_batches.commit", "cannot commit export batch: %v", err)
	}
	return nil
}

func (c *Checkpoint) LoadState(ctx context.Context, expectedProjectVersion string) (*CheckpointState, error) {
	if err := c.verifyIntegrity(); err != nil {
		return nil, err
	}
	state := &CheckpointState{}
	var err error
	if state.ProjectID, err = c.meta(ctx, "project_id"); err != nil {
		return nil, err
	}
	if state.ProjectVersion, err = c.meta(ctx, "project_version"); err != nil {
		return nil, err
	}
	if state.ProjectFingerprint, err = c.meta(ctx, "project_fingerprint"); err != nil {
		return nil, err
	}
	if value, err := c.meta(ctx, "global_commit_seq"); err != nil {
		return nil, err
	} else if value != "" {
		if _, err := scanInt64(value, &state.GlobalCommitSeq); err != nil {
			return nil, NewError(CodeCheckpointCorrupt, "checkpoint_meta.global_commit_seq", "invalid sequence: %v", err).WithContext("value", value)
		}
	}
	if value, err := c.meta(ctx, "last_export_batch_no"); err != nil {
		return nil, err
	} else if value != "" {
		state.LastExportBatchNo = atoiOrZero(value)
	}

	if expectedProjectVersion != "" && state.ProjectVersion != "" && state.ProjectVersion != expectedProjectVersion {
		return nil, NewError(CodeCheckpointVersion, "checkpoint_meta.project_version", "checkpoint project version does not match input").
			WithContext("checkpoint_version", state.ProjectVersion).WithContext("expected_version", expectedProjectVersion)
	}

	rows, err := c.db.QueryContext(ctx, `SELECT payload_json FROM window_states WHERE commit_seq <= ? ORDER BY window_key`, state.GlobalCommitSeq)
	if err != nil {
		return nil, NewError(CodeCheckpointCorrupt, "window_states", "cannot query committed windows: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, NewError(CodeCheckpointCorrupt, "window_states.payload_json", "cannot scan window: %v", err)
		}
		var agg Aggregate
		if err := json.Unmarshal([]byte(payload), &agg); err != nil {
			return nil, NewError(CodeCheckpointCorrupt, "window_states.payload_json", "cannot decode window: %v", err)
		}
		state.Windows = append(state.Windows, &agg)
	}
	if err := rows.Err(); err != nil {
		return nil, NewError(CodeCheckpointCorrupt, "window_states", "window iteration failed: %v", err)
	}

	batchRows, err := c.db.QueryContext(ctx, `SELECT payload_json FROM export_batches WHERE status='committed' ORDER BY batch_no`)
	if err != nil {
		return nil, NewError(CodeCheckpointCorrupt, "export_batches", "cannot query batches: %v", err)
	}
	defer batchRows.Close()
	for batchRows.Next() {
		var payload string
		if err := batchRows.Scan(&payload); err != nil {
			return nil, NewError(CodeCheckpointCorrupt, "export_batches.payload_json", "cannot scan batch: %v", err)
		}
		var batch ExportBatch
		if err := json.Unmarshal([]byte(payload), &batch); err != nil {
			return nil, NewError(CodeCheckpointCorrupt, "export_batches.payload_json", "cannot decode batch: %v", err)
		}
		state.Batches = append(state.Batches, &batch)
	}
	return state, batchRows.Err()
}

func formatInt64(v int64) string { return strconv.FormatInt(v, 10) }

func scanInt64(s string, target *int64) (int, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	*target = v
	return len(s), nil
}

func atoiOrZero(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

func formatInt(v int) string { return strconv.Itoa(v) }
