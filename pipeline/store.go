package pipeline

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

const storeSchema = `
CREATE TABLE IF NOT EXISTS fp_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS fp_windows (
    feature_id TEXT NOT NULL,
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    status TEXT NOT NULL,
    accepted_seqs TEXT NOT NULL,
    aggregates TEXT NOT NULL,
    recompute_count INTEGER NOT NULL,
    updated_seq INTEGER NOT NULL,
    PRIMARY KEY (feature_id, start_ms)
);
CREATE TABLE IF NOT EXISTS fp_discards (
    event_seq INTEGER NOT NULL,
    feature_id TEXT NOT NULL,
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    observed_at_ms INTEGER NOT NULL,
    watermark_ms INTEGER NOT NULL,
    late_by_ms INTEGER NOT NULL,
    reason_code TEXT NOT NULL,
    PRIMARY KEY (event_seq, feature_id, start_ms)
);
`

// CheckpointStore is the pure-Go SQLite persistence adapter.
type CheckpointStore struct {
	db   *sql.DB
	path string
}

// OpenCheckpointStore opens or creates a local SQLite database. The connection
// pool is intentionally capped at one writer; SQLite serializes writers and
// deterministic checkpoint ordering is more important than throughput here.
func OpenCheckpointStore(path string) (*CheckpointStore, error) {
	if path == "" {
		return nil, NewBusinessError(CodeCheckpointStore, "checkpoint path is empty")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, checkpointError("mkdir", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, checkpointError("open", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &CheckpointStore{db: db, path: path}
	if err := store.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) initialize() error {
	if _, err := s.db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return checkpointError("pragma_busy_timeout", err)
	}
	if s.path != ":memory:" {
		// WAL keeps readers useful during a forced producer termination.
		if _, err := s.db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
			return checkpointError("pragma_journal", err)
		}
	}
	if _, err := s.db.Exec(`PRAGMA synchronous = FULL`); err != nil {
		return checkpointError("pragma_synchronous", err)
	}
	if _, err := s.db.Exec(storeSchema); err != nil {
		return checkpointError("schema", err)
	}
	return nil
}

// Close releases the SQLite handle.
func (s *CheckpointStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the backing database path.
func (s *CheckpointStore) Path() string { return s.path }

// Reset clears all checkpoint data. It does not touch source project documents.
func (s *CheckpointStore) Reset() error {
	tx, err := s.db.Begin()
	if err != nil {
		return checkpointError("reset_begin", err)
	}
	defer tx.Rollback()
	for _, table := range []string{"fp_windows", "fp_discards", "fp_meta"} {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return checkpointError("reset_"+table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return checkpointError("reset_commit", err)
	}
	return nil
}

// Load validates metadata before reading rows. A missing metadata set is an
// empty store; a partial metadata set is a truncated checkpoint.
func (s *CheckpointStore) Load(project *Project, fingerprint string) (*CheckpointSnapshot, ResumeInfo, error) {
	meta, err := s.readMeta()
	if err != nil {
		return nil, ResumeInfo{}, err
	}
	if len(meta) == 0 {
		return nil, ResumeInfo{Found: false, Compatible: true}, nil
	}
	required := []string{
		"engine_version", "project_fingerprint", "project_id", "schema_version",
		"last_event_seq", "watermark_enabled",
	}
	for _, key := range required {
		if _, ok := meta[key]; !ok {
			return nil, ResumeInfo{}, NewBusinessError(CodeCheckpointTruncated, "checkpoint metadata is incomplete").
				WithContext("missing_key", key).WithContext("table", "fp_meta")
		}
	}
	if meta["engine_version"] != EngineVersion || meta["project_fingerprint"] != fingerprint {
		if err := s.Reset(); err != nil {
			return nil, ResumeInfo{}, err
		}
		return nil, ResumeInfo{
			Found: true, Compatible: false, FullRebuild: true,
			ReasonCode: CodeCheckpointVersionMismatch,
			Note:       "checkpoint version/fingerprint mismatch; clearing and rebuilding from event 1",
		}, nil
	}
	lastSeq, err := strconv.ParseInt(meta["last_event_seq"], 10, 64)
	if err != nil || lastSeq < 0 || lastSeq > int64(len(project.Events)) {
		return nil, ResumeInfo{}, NewBusinessError(CodeCheckpointTruncated, "checkpoint last_event_seq is invalid").
			WithContext("last_event_seq", meta["last_event_seq"]).
			WithContext("event_count", fmt.Sprint(len(project.Events)))
	}
	if meta["project_id"] != project.ProjectID {
		return nil, ResumeInfo{}, NewBusinessError(CodeCheckpointTruncated, "checkpoint project id does not match").
			WithContext("checkpoint_project", meta["project_id"]).
			WithContext("project", project.ProjectID)
	}
	if meta["schema_version"] != project.SchemaVersion {
		return nil, ResumeInfo{}, NewBusinessError(CodeCheckpointVersionMismatch, "checkpoint schema version mismatch").
			WithContext("checkpoint_schema", meta["schema_version"]).
			WithContext("project_schema", project.SchemaVersion)
	}
	enabled, err := strconv.ParseBool(meta["watermark_enabled"])
	if err != nil {
		return nil, ResumeInfo{}, NewBusinessError(CodeCheckpointTruncated, "checkpoint watermark flag is invalid").
			WithContext("value", meta["watermark_enabled"])
	}
	snapshot := &CheckpointSnapshot{
		EngineVersion: EngineVersion, ProjectFingerprint: fingerprint,
		ProjectID: project.ProjectID, SchemaVersion: project.SchemaVersion,
		LastEventSeq: lastSeq, WatermarkEnabled: enabled,
	}
	if wmText, ok := meta["watermark_ms"]; ok && wmText != "" {
		wm, err := strconv.ParseInt(wmText, 10, 64)
		if err != nil {
			return nil, ResumeInfo{}, NewBusinessError(CodeCheckpointTruncated, "checkpoint watermark is invalid").
				WithContext("value", wmText)
		}
		snapshot.WatermarkMs = &wm
	}
	if err := s.loadWindows(snapshot, lastSeq); err != nil {
		return nil, ResumeInfo{}, err
	}
	if err := s.loadDiscards(snapshot, lastSeq); err != nil {
		return nil, ResumeInfo{}, err
	}
	return snapshot, ResumeInfo{
		Found: true, Compatible: true, LastSeq: lastSeq,
		Note: fmt.Sprintf("resumed compatible checkpoint at event %d", lastSeq),
	}, nil
}

func (s *CheckpointStore) readMeta() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM fp_meta`)
	if err != nil {
		return nil, checkpointError("load_meta", err)
	}
	defer rows.Close()
	meta := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, checkpointError("scan_meta", err)
		}
		meta[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, checkpointError("iterate_meta", err)
	}
	return meta, nil
}

func (s *CheckpointStore) loadWindows(snapshot *CheckpointSnapshot, lastSeq int64) error {
	rows, err := s.db.Query(`SELECT feature_id, start_ms, end_ms, status, accepted_seqs, aggregates, recompute_count, updated_seq
		FROM fp_windows ORDER BY feature_id, start_ms`)
	if err != nil {
		return checkpointError("load_windows", err)
	}
	defer rows.Close()
	for rows.Next() {
		var featureID, status, seqJSON, aggJSON string
		var start, end, recompute, updated int64
		if err := rows.Scan(&featureID, &start, &end, &status, &seqJSON, &aggJSON, &recompute, &updated); err != nil {
			return checkpointError("scan_window", err)
		}
		if updated > lastSeq {
			return NewBusinessError(CodeCheckpointTruncated, "window references a future event sequence").
				WithContext("feature_id", featureID).WithContext("start_ms", fmt.Sprint(start)).
				WithContext("updated_seq", fmt.Sprint(updated)).
				WithContext("last_event_seq", fmt.Sprint(lastSeq))
		}
		var seqs []int64
		if err := json.Unmarshal([]byte(seqJSON), &seqs); err != nil {
			return NewBusinessError(CodeCheckpointTruncated, "window accepted_seqs is corrupt").
				WithContext("feature_id", featureID).WithContext("start_ms", fmt.Sprint(start)).
				WithContext("json_error", err.Error())
		}
		for _, seq := range seqs {
			if seq > lastSeq {
				return NewBusinessError(CodeCheckpointTruncated, "window accepted event is beyond checkpoint sequence").
					WithContext("feature_id", featureID).WithContext("event_seq", fmt.Sprint(seq)).
					WithContext("last_event_seq", fmt.Sprint(lastSeq))
			}
		}
		var aggregates []AggregateResult
		if err := json.Unmarshal([]byte(aggJSON), &aggregates); err != nil {
			return NewBusinessError(CodeCheckpointTruncated, "window aggregates are corrupt").
				WithContext("feature_id", featureID).WithContext("start_ms", fmt.Sprint(start)).
				WithContext("json_error", err.Error())
		}
		wsStatus := WindowStatus(status)
		if wsStatus != WindowOpen && wsStatus != WindowCommitted {
			return NewBusinessError(CodeCheckpointTruncated, "window status is invalid").
				WithContext("feature_id", featureID).WithContext("status", status)
		}
		snapshot.Windows = append(snapshot.Windows, PersistedWindow{
			Key:    WindowKey{FeatureID: featureID, StartMs: start, EndMs: end},
			Status: wsStatus, AcceptedSeqs: seqs, Aggregates: aggregates,
			RecomputeCount: recompute,
		})
	}
	if err := rows.Err(); err != nil {
		return checkpointError("iterate_windows", err)
	}
	return nil
}

func (s *CheckpointStore) loadDiscards(snapshot *CheckpointSnapshot, lastSeq int64) error {
	rows, err := s.db.Query(`SELECT event_seq, feature_id, start_ms, end_ms, observed_at_ms, watermark_ms, late_by_ms, reason_code
		FROM fp_discards ORDER BY event_seq, feature_id, start_ms`)
	if err != nil {
		return checkpointError("load_discards", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d DiscardRecord
		if err := rows.Scan(&d.EventSeq, &d.FeatureID, &d.Window.StartMs, &d.Window.EndMs,
			&d.ObservedAtMs, &d.WatermarkMs, &d.LateByMs, &d.ReasonCode); err != nil {
			return checkpointError("scan_discard", err)
		}
		if d.EventSeq > lastSeq {
			return NewBusinessError(CodeCheckpointTruncated, "discard is beyond checkpoint sequence").
				WithContext("event_seq", fmt.Sprint(d.EventSeq)).
				WithContext("last_event_seq", fmt.Sprint(lastSeq))
		}
		d.Window.FeatureID = d.FeatureID
		snapshot.Discards = append(snapshot.Discards, d)
	}
	if err := rows.Err(); err != nil {
		return checkpointError("iterate_discards", err)
	}
	return nil
}

// Save replaces the checkpoint snapshot in one transaction. Windows and
// discards are written first; last_event_seq is the final metadata mutation.
// A crash before COMMIT therefore cannot expose a half-written window.
func (s *CheckpointStore) Save(snapshot *CheckpointSnapshot) error {
	if snapshot == nil {
		return NewBusinessError(CodeCheckpointStore, "nil checkpoint snapshot")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return checkpointError("save_begin", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM fp_windows`); err != nil {
		return checkpointError("clear_windows", err)
	}
	if _, err := tx.Exec(`DELETE FROM fp_discards`); err != nil {
		return checkpointError("clear_discards", err)
	}
	for _, w := range snapshot.Windows {
		seqJSON, err := json.Marshal(w.AcceptedSeqs)
		if err != nil {
			return checkpointError("encode_accepted_seqs", err)
		}
		aggJSON, err := json.Marshal(w.Aggregates)
		if err != nil {
			return checkpointError("encode_aggregates", err)
		}
		if _, err := tx.Exec(`INSERT INTO fp_windows
			(feature_id, start_ms, end_ms, status, accepted_seqs, aggregates, recompute_count, updated_seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			w.Key.FeatureID, w.Key.StartMs, w.Key.EndMs, string(w.Status),
			string(seqJSON), string(aggJSON), w.RecomputeCount, snapshot.LastEventSeq); err != nil {
			return checkpointError("insert_window", err)
		}
	}
	for _, d := range snapshot.Discards {
		if _, err := tx.Exec(`INSERT INTO fp_discards
			(event_seq, feature_id, start_ms, end_ms, observed_at_ms, watermark_ms, late_by_ms, reason_code)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			d.EventSeq, d.FeatureID, d.Window.StartMs, d.Window.EndMs,
			d.ObservedAtMs, d.WatermarkMs, d.LateByMs, d.ReasonCode); err != nil {
			return checkpointError("insert_discard", err)
		}
	}
	meta := map[string]string{
		"engine_version":      EngineVersion,
		"project_fingerprint": snapshot.ProjectFingerprint,
		"project_id":          snapshot.ProjectID,
		"schema_version":      snapshot.SchemaVersion,
		"watermark_enabled":   strconv.FormatBool(snapshot.WatermarkEnabled),
	}
	if snapshot.WatermarkMs != nil {
		meta["watermark_ms"] = strconv.FormatInt(*snapshot.WatermarkMs, 10)
	}
	for key, value := range meta {
		if _, err := tx.Exec(`INSERT INTO fp_meta(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return checkpointError("upsert_meta_"+key, err)
		}
	}
	// Deliberately last: readers never observe a new global sequence with
	// window rows from an older batch.
	if _, err := tx.Exec(`INSERT INTO fp_meta(key, value) VALUES('last_event_seq', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.FormatInt(snapshot.LastEventSeq, 10)); err != nil {
		return checkpointError("upsert_last_event_seq", err)
	}
	if err := tx.Commit(); err != nil {
		return checkpointError("save_commit", err)
	}
	return nil
}

// Metadata returns checkpoint metadata for diagnostics.
func (s *CheckpointStore) Metadata() (map[string]string, error) {
	return s.readMeta()
}

// InjectVersionMismatch changes metadata only; it does not modify source
// project input.
func (s *CheckpointStore) InjectVersionMismatch() error {
	_, err := s.db.Exec(`INSERT INTO fp_meta(key, value) VALUES('project_fingerprint', 'injected-stale-version')
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	if err != nil {
		return checkpointError("inject_version_mismatch", err)
	}
	return nil
}

// InjectTruncatedCheckpoint removes a required metadata key to model a
// damaged/truncated checkpoint.
func (s *CheckpointStore) InjectTruncatedCheckpoint() error {
	if _, err := s.db.Exec(`DELETE FROM fp_meta WHERE key IN ('engine_version', 'project_fingerprint')`); err != nil {
		return checkpointError("inject_truncated_checkpoint", err)
	}
	return nil
}

// CheckpointCounts reports persisted row counts.
func (s *CheckpointStore) CheckpointCounts() (windows, discards int, err error) {
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM fp_windows`).Scan(&windows); err != nil {
		return 0, 0, checkpointError("count_windows", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM fp_discards`).Scan(&discards); err != nil {
		return 0, 0, checkpointError("count_discards", err)
	}
	return windows, discards, nil
}

// SimulateCrashBeforeCommit writes window rows in a transaction and rolls the
// transaction back, proving that the global event sequence is not advanced.
func (s *CheckpointStore) SimulateCrashBeforeCommit(snapshot *CheckpointSnapshot) error {
	if snapshot == nil {
		return NewBusinessError(CodeCheckpointStore, "nil checkpoint snapshot")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return checkpointError("crash_begin", err)
	}
	_, execErr := tx.Exec(`UPDATE fp_windows SET updated_seq = ?`, snapshot.LastEventSeq)
	rollbackErr := tx.Rollback()
	if execErr != nil {
		return checkpointError("crash_write_window", execErr)
	}
	if rollbackErr != nil {
		return checkpointError("crash_rollback", rollbackErr)
	}
	return nil
}
