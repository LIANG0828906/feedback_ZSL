package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	_ "modernc.org/sqlite"

	"featurekernel/internal/engine"
	"featurekernel/internal/kernel"
)

const (
	CheckpointVersionV1 = "checkpoint.v1"
	DriverName          = "sqlite"
)

type Store struct {
	db   *sql.DB
	path string

	CrashAfterWindowStates func()
}

type Snapshot struct {
	CheckpointVersion string                          `json:"checkpoint_version"`
	CanonicalVersion  string                          `json:"canonical_version"`
	ProjectID         string                          `json:"project_id"`
	BatchID           string                          `json:"batch_id"`
	Cursor            int64                           `json:"cursor"`
	FeatureStates     map[string][]engine.WindowState `json:"feature_states"`
	Watermarks        []kernel.Watermark              `json:"watermarks"`
	Discarded         []kernel.DiscardedRecord        `json:"discarded"`
	Recomputations    []kernel.Recomputation          `json:"recomputations"`
}

func OpenSQLite(ctx context.Context, path string) (*Store, error) {
	if err := inspectSQLiteFile(path); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointOpen, err, map[string]any{"path": path})
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		code := kernel.CodeCheckpointOpen
		if isCorruptError(err) {
			code = kernel.CodeCheckpointCorrupt
		}
		return nil, kernel.WrapError(code, err, map[string]any{"path": path, "stage": "ping"})
	}
	store := &Store{db: db, path: path}
	if err := store.initSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func inspectSQLiteFile(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return kernel.WrapError(kernel.CodeCheckpointOpen, err, map[string]any{"path": path})
	}
	if info.IsDir() {
		return kernel.NewError(kernel.CodeCheckpointOpen, "checkpoint path is a directory", map[string]any{"path": path})
	}
	if info.Size() < 100 {
		return kernel.NewError(kernel.CodeCheckpointTruncated, "sqlite checkpoint is shorter than 100-byte header", map[string]any{"path": path, "size": info.Size()})
	}
	file, err := os.Open(path)
	if err != nil {
		return kernel.WrapError(kernel.CodeCheckpointOpen, err, map[string]any{"path": path})
	}
	defer file.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(file, header); err != nil {
		return kernel.WrapError(kernel.CodeCheckpointTruncated, err, map[string]any{"path": path, "stage": "read_header"})
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return kernel.NewError(kernel.CodeCheckpointCorrupt, "invalid sqlite magic", map[string]any{"path": path})
	}
	pageSize := int64(uint16(header[16])<<8 | uint16(header[17]))
	if pageSize == 1 {
		pageSize = 65536
	}
	pageCount := int64(uint32(header[28])<<24 | uint32(header[29])<<16 | uint32(header[30])<<8 | uint32(header[31]))
	if pageSize > 0 && pageCount > 0 {
		expectedSize := pageSize * pageCount
		if info.Size() < expectedSize {
			return kernel.NewError(kernel.CodeCheckpointTruncated, "sqlite file is shorter than header page count", map[string]any{
				"path": path, "actual_size": info.Size(), "expected_size": expectedSize, "page_count": pageCount, "page_size": pageSize,
			})
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) initSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS global_cursor (id INTEGER PRIMARY KEY CHECK (id = 1), seq INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO global_cursor(id, seq) VALUES (1, 0)`,
		`CREATE TABLE IF NOT EXISTS window_states (
			project_id TEXT NOT NULL,
			feature_id TEXT NOT NULL,
			window_index INTEGER NOT NULL,
			window_id TEXT NOT NULL,
			start_ms INTEGER NOT NULL,
			end_ms INTEGER NOT NULL,
			state_json TEXT NOT NULL,
			submitted INTEGER NOT NULL,
			dirty_seq INTEGER NOT NULL,
			updated_seq INTEGER NOT NULL,
			PRIMARY KEY(project_id, feature_id, window_index)
		)`,
		`CREATE TABLE IF NOT EXISTS watermarks (
			project_id TEXT NOT NULL,
			arrival_seq INTEGER NOT NULL,
			time_ms INTEGER NOT NULL,
			PRIMARY KEY(project_id, arrival_seq)
		)`,
		`CREATE TABLE IF NOT EXISTS discards (
			project_id TEXT NOT NULL,
			arrival_seq INTEGER NOT NULL,
			feature_id TEXT NOT NULL,
			window_index INTEGER NOT NULL,
			late_ms INTEGER NOT NULL,
			code TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			PRIMARY KEY(project_id, arrival_seq, feature_id, window_index)
		)`,
		`CREATE TABLE IF NOT EXISTS recomputations (
			project_id TEXT NOT NULL,
			trigger_seq INTEGER NOT NULL,
			feature_id TEXT NOT NULL,
			window_index INTEGER NOT NULL,
			window_id TEXT NOT NULL,
			late_ms INTEGER NOT NULL,
			payload_json TEXT NOT NULL,
			PRIMARY KEY(project_id, trigger_seq, feature_id, window_index)
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			code := kernel.CodeCheckpointOpen
			if isCorruptError(err) {
				code = kernel.CodeCheckpointCorrupt
			}
			return kernel.WrapError(code, err, map[string]any{"stage": "init_schema"})
		}
	}
	return nil
}

func isCorruptError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "malformed") ||
		strings.Contains(message, "not a database") ||
		strings.Contains(message, "disk image") ||
		errors.Is(err, os.ErrInvalid)
}

func (s *Store) LoadSnapshot(ctx context.Context, projectID string) (*Snapshot, error) {
	snapshot := &Snapshot{FeatureStates: map[string][]engine.WindowState{}}

	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM checkpoint_meta`)
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"stage": "load_meta"})
	}
	meta := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			_ = rows.Close()
			return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"stage": "scan_meta"})
		}
		meta[key] = value
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"stage": "meta_rows"})
	}

	snapshot.CheckpointVersion = meta["checkpoint_version"]
	snapshot.CanonicalVersion = meta["canonical_version"]
	snapshot.ProjectID = meta["project_id"]
	snapshot.BatchID = meta["batch_id"]
	if snapshot.CheckpointVersion != "" && snapshot.CheckpointVersion != CheckpointVersionV1 {
		return nil, kernel.NewError(kernel.CodeCheckpointVersionMismatch, "checkpoint format version does not match", map[string]any{
			"stored": snapshot.CheckpointVersion, "current": CheckpointVersionV1,
		})
	}
	if snapshot.CanonicalVersion != "" && snapshot.CanonicalVersion != kernel.CanonicalVersion {
		return nil, kernel.NewError(kernel.CodeCheckpointVersionMismatch, "checkpoint canonical version does not match; full recomputation required", map[string]any{
			"stored": snapshot.CanonicalVersion, "current": kernel.CanonicalVersion,
		})
	}
	if snapshot.ProjectID != "" && projectID != "" && snapshot.ProjectID != projectID {
		return nil, kernel.NewError(kernel.CodeCheckpointVersionMismatch, "checkpoint project id does not match", map[string]any{
			"stored": snapshot.ProjectID, "requested": projectID,
		})
	}

	if err := s.db.QueryRowContext(ctx, `SELECT seq FROM global_cursor WHERE id = 1`).Scan(&snapshot.Cursor); err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"stage": "load_cursor"})
	}
	if snapshot.Cursor == 0 && snapshot.CheckpointVersion == "" {
		return snapshot, nil
	}
	if snapshot.CheckpointVersion == "" {
		return nil, kernel.NewError(kernel.CodeCheckpointTruncated, "checkpoint cursor exists without version metadata", map[string]any{"cursor": snapshot.Cursor})
	}

	windowRows, err := s.db.QueryContext(ctx, `SELECT feature_id, state_json FROM window_states WHERE project_id = ? ORDER BY feature_id, window_index`, projectID)
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"stage": "load_windows"})
	}
	defer windowRows.Close()
	for windowRows.Next() {
		var featureID, payload string
		if err := windowRows.Scan(&featureID, &payload); err != nil {
			return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, nil)
		}
		var state engine.WindowState
		if err := json.Unmarshal([]byte(payload), &state); err != nil {
			return nil, kernel.WrapError(kernel.CodeCheckpointTruncated, err, map[string]any{"feature_id": featureID})
		}
		snapshot.FeatureStates[featureID] = append(snapshot.FeatureStates[featureID], state)
	}
	if err := windowRows.Err(); err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, nil)
	}

	watermarkRows, err := s.db.QueryContext(ctx, `SELECT arrival_seq, time_ms FROM watermarks WHERE project_id = ? ORDER BY arrival_seq`, projectID)
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"stage": "load_watermarks"})
	}
	defer watermarkRows.Close()
	for watermarkRows.Next() {
		var wm kernel.Watermark
		if err := watermarkRows.Scan(&wm.ArrivalSeq, &wm.TimeMs); err != nil {
			return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, nil)
		}
		snapshot.Watermarks = append(snapshot.Watermarks, wm)
	}

	discardRows, err := s.db.QueryContext(ctx, `SELECT payload_json FROM discards WHERE project_id = ? ORDER BY arrival_seq, feature_id, window_index`, projectID)
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"stage": "load_discards"})
	}
	defer discardRows.Close()
	for discardRows.Next() {
		var payload string
		if err := discardRows.Scan(&payload); err != nil {
			return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, nil)
		}
		var record kernel.DiscardedRecord
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return nil, kernel.WrapError(kernel.CodeCheckpointTruncated, err, map[string]any{"stage": "discard_json"})
		}
		snapshot.Discarded = append(snapshot.Discarded, record)
	}

	recomputeRows, err := s.db.QueryContext(ctx, `SELECT payload_json FROM recomputations WHERE project_id = ? ORDER BY trigger_seq, feature_id, window_index`, projectID)
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"stage": "load_recomputations"})
	}
	defer recomputeRows.Close()
	for recomputeRows.Next() {
		var payload string
		if err := recomputeRows.Scan(&payload); err != nil {
			return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, nil)
		}
		var record kernel.Recomputation
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return nil, kernel.WrapError(kernel.CodeCheckpointTruncated, err, map[string]any{"stage": "recomputation_json"})
		}
		snapshot.Recomputations = append(snapshot.Recomputations, record)
	}
	return snapshot, nil
}

type CommitData struct {
	ProjectID  string
	BatchID    string
	Cursor     int64
	Runtimes   []*engine.Runtime
	Watermarks []kernel.Watermark
}

func (s *Store) Commit(ctx context.Context, data CommitData) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "begin"})
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, runtime := range data.Runtimes {
		if runtime == nil {
			continue
		}
		for _, state := range runtime.States {
			payload, err := json.Marshal(state)
			if err != nil {
				return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "marshal_window"})
			}
			submitted := 0
			if state.Submitted {
				submitted = 1
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO window_states(project_id, feature_id, window_index, window_id, start_ms, end_ms, state_json, submitted, dirty_seq, updated_seq)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(project_id, feature_id, window_index) DO UPDATE SET
					window_id=excluded.window_id, start_ms=excluded.start_ms, end_ms=excluded.end_ms,
					state_json=excluded.state_json, submitted=excluded.submitted,
					dirty_seq=excluded.dirty_seq, updated_seq=excluded.updated_seq`,
				data.ProjectID, runtime.Feature.ID, state.Coord.Index,
				kernel.WindowID(runtime.Feature.ID, state.Coord.Index), state.Coord.Start, state.Coord.End,
				string(payload), submitted, state.DirtySeq, data.Cursor,
			)
			if err != nil {
				return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "upsert_window", "window": state.Coord.Index})
			}
		}
	}

	if s.CrashAfterWindowStates != nil {
		s.CrashAfterWindowStates()
	}

	meta := map[string]string{
		"checkpoint_version": CheckpointVersionV1,
		"canonical_version":  kernel.CanonicalVersion,
		"project_id":         data.ProjectID,
		"batch_id":           data.BatchID,
	}
	for key, value := range meta {
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "upsert_meta", "key": key})
		}
	}
	for _, wm := range data.Watermarks {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO watermarks(project_id, arrival_seq, time_ms) VALUES(?, ?, ?)`, data.ProjectID, wm.ArrivalSeq, wm.TimeMs); err != nil {
			return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "insert_watermark"})
		}
	}
	for _, runtime := range data.Runtimes {
		if runtime == nil {
			continue
		}
		for _, item := range runtime.Discarded {
			payload, _ := json.Marshal(item)
			_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO discards(project_id, arrival_seq, feature_id, window_index, late_ms, code, payload_json) VALUES(?, ?, ?, ?, ?, ?, ?)`,
				data.ProjectID, item.ArrivalSeq, item.FeatureID, item.WindowIndex, item.LateMs, item.Code, string(payload))
			if err != nil {
				return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "insert_discard"})
			}
		}
		for _, item := range runtime.Recomputations {
			payload, _ := json.Marshal(item)
			_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO recomputations(project_id, trigger_seq, feature_id, window_index, window_id, late_ms, payload_json) VALUES(?, ?, ?, ?, ?, ?, ?)`,
				data.ProjectID, item.TriggerSeq, item.FeatureID, item.WindowIndex, item.WindowID, item.LateMs, string(payload))
			if err != nil {
				return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "insert_recomputation"})
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE global_cursor SET seq = ? WHERE id = 1`, data.Cursor); err != nil {
		return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "update_cursor"})
	}
	if err := tx.Commit(); err != nil {
		return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "commit"})
	}
	committed = true
	return nil
}

func (s *Store) Reset(ctx context.Context, projectID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "reset_begin"})
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM window_states WHERE project_id = ?`,
		`DELETE FROM watermarks WHERE project_id = ?`,
		`DELETE FROM discards WHERE project_id = ?`,
		`DELETE FROM recomputations WHERE project_id = ?`,
		`DELETE FROM checkpoint_meta`,
		`UPDATE global_cursor SET seq = 0 WHERE id = 1`,
	} {
		if _, err := tx.ExecContext(ctx, statement, projectID); err != nil {
			return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "reset"})
		}
	}
	if err := tx.Commit(); err != nil {
		return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "reset_commit"})
	}
	return nil
}

func (s *Store) ExportSnapshot(ctx context.Context, projectID, batchID string) (*Snapshot, error) {
	snapshot, err := s.LoadSnapshot(ctx, projectID)
	if err != nil {
		return nil, err
	}
	snapshot.BatchID = batchID
	return snapshot, nil
}

func EnsureSQLiteDriver() string { return fmt.Sprintf("loaded:%s", DriverName) }
