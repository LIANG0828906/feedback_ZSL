package downsample

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

type Checkpoint struct {
	db   *sql.DB
	path string
}

type Recovery struct {
	Found           bool                       `json:"found"`
	GlobalSeq       int64                      `json:"global_seq"`
	ConfigVersion   string                     `json:"config_version"`
	ExpectedVersion string                     `json:"expected_version"`
	Results         map[WindowKey]WindowResult `json:"-"`
	Points          []Point                    `json:"points"`
	OrphanRows      int                        `json:"orphan_rows"`
}

func OpenCheckpoint(path string) (*Checkpoint, *Error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil, NewError(CodeCheckpointOpen, "checkpoint path is a directory").
			WithFieldPath(path)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, WrapError(CodeCheckpointOpen, "cannot open sqlite checkpoint", err).
			WithContext("path", path)
	}
	db.SetMaxOpenConns(1)
	c := &Checkpoint{db: db, path: path}
	if err := c.createSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := c.checkIntegrity(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Checkpoint) Path() string { return c.path }

func (c *Checkpoint) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

func (c *Checkpoint) createSchema() *Error {
	_, err := c.db.Exec(`
CREATE TABLE IF NOT EXISTS checkpoint_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS window_states (
  metric_id TEXT NOT NULL,
  start_ms INTEGER NOT NULL,
  window_index INTEGER NOT NULL,
  config_version TEXT NOT NULL,
  commit_seq INTEGER NOT NULL,
  result_json TEXT NOT NULL,
  ready INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY(metric_id, start_ms, config_version)
);
CREATE INDEX IF NOT EXISTS idx_window_commit_seq ON window_states(commit_seq);
CREATE TABLE IF NOT EXISTS samples (
  seq INTEGER PRIMARY KEY,
  metric_id TEXT NOT NULL,
  timestamp_ms INTEGER NOT NULL,
  value REAL NOT NULL,
  weight INTEGER NOT NULL,
  point_json TEXT NOT NULL,
  commit_seq INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples_commit_seq ON samples(commit_seq);
`)
	if err != nil {
		code := CodeCheckpointOpen
		if looksTruncated(err.Error()) {
			code = CodeCheckpointTruncated
		}
		return WrapError(code, "cannot create checkpoint schema", err).
			WithContext("path", c.path)
	}
	return nil
}

func (c *Checkpoint) checkIntegrity() *Error {
	var result string
	err := c.db.QueryRow("PRAGMA integrity_check").Scan(&result)
	if err != nil {
		code := CodeCheckpointOpen
		if looksTruncated(err.Error()) {
			code = CodeCheckpointTruncated
		}
		return WrapError(code, "checkpoint integrity check failed", err).
			WithContext("path", c.path)
	}
	if strings.ToLower(strings.TrimSpace(result)) != "ok" {
		return NewError(CodeCheckpointTruncated, "checkpoint integrity_check reported corruption").
			WithContext("path", c.path).
			WithContext("integrity", result)
	}
	return nil
}

func looksTruncated(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "malformed") ||
		strings.Contains(message, "truncated") ||
		strings.Contains(message, "not a database") ||
		strings.Contains(message, "disk image")
}

// SaveWindowStates is phase 1: all complete window states are committed before
// the global logical sequence pointer moves.
func (c *Checkpoint) SaveWindowStates(configVersion string, commitSeq int64, results []WindowResult) *Error {
	return c.savePhaseOne(configVersion, commitSeq, results, nil)
}

func (c *Checkpoint) SaveCommitBatch(configVersion string, seq int64, results []WindowResult, points []Point) *Error {
	if err := c.savePhaseOne(configVersion, seq, results, points); err != nil {
		return err
	}
	return c.CommitGlobalSequence(configVersion, seq)
}

func (c *Checkpoint) savePhaseOne(configVersion string, commitSeq int64, results []WindowResult, points []Point) *Error {
	tx, err := c.db.Begin()
	if err != nil {
		return WrapError(CodeCheckpointCommitOrder, "cannot begin window-state transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(points) > 0 {
		sampleStmt, perr := tx.Prepare(`
INSERT INTO samples(seq,metric_id,timestamp_ms,value,weight,point_json,commit_seq)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(seq) DO UPDATE SET
  metric_id=excluded.metric_id,
  timestamp_ms=excluded.timestamp_ms,
  value=excluded.value,
  weight=excluded.weight,
  point_json=excluded.point_json,
  commit_seq=excluded.commit_seq
`)
		if perr != nil {
			return WrapError(CodeCheckpointCommitOrder, "cannot prepare sample upsert", perr)
		}
		defer sampleStmt.Close()
		for _, point := range points {
			payload, jerr := json.Marshal(point)
			if jerr != nil {
				return WrapError(CodeCheckpointCommitOrder, "cannot encode sample", jerr).
					WithContext("seq", point.Seq)
			}
			if _, err := sampleStmt.Exec(point.Seq, point.MetricID, point.TimestampMS, point.Value, point.Weight, string(payload), commitSeq); err != nil {
				return WrapError(CodeCheckpointCommitOrder, "cannot write sample state", err).
					WithContext("seq", point.Seq).
					WithContext("commit_seq", commitSeq)
			}
		}
	}

	stmt, err := tx.Prepare(`
INSERT INTO window_states(metric_id,start_ms,window_index,config_version,commit_seq,result_json,ready)
VALUES(?,?,?,?,?,?,1)
ON CONFLICT(metric_id,start_ms,config_version) DO UPDATE SET
  window_index=excluded.window_index,
  commit_seq=excluded.commit_seq,
  result_json=excluded.result_json,
  ready=1
`)
	if err != nil {
		return WrapError(CodeCheckpointCommitOrder, "cannot prepare window-state upsert", err)
	}
	defer stmt.Close()

	for _, result := range results {
		payload, jerr := json.Marshal(result)
		if jerr != nil {
			return WrapError(CodeCheckpointCommitOrder, "cannot encode window result", jerr).
				WithContext("window", result.Key.String())
		}
		if _, err := stmt.Exec(result.MetricID, result.StartMS, result.WindowIndex, configVersion, commitSeq, string(payload)); err != nil {
			return WrapError(CodeCheckpointCommitOrder, "cannot write window state", err).
				WithContext("window", result.Key.String()).
				WithContext("commit_seq", commitSeq)
		}
	}
	if err := tx.Commit(); err != nil {
		return WrapError(CodeCheckpointCommitOrder, "cannot commit window states", err).
			WithContext("commit_seq", commitSeq)
	}
	return nil
}

// CommitGlobalSequence is phase 2 and is the only point where recovery sees a
// new globally acknowledged sequence.
func (c *Checkpoint) CommitGlobalSequence(configVersion string, seq int64) *Error {
	tx, err := c.db.Begin()
	if err != nil {
		return WrapError(CodeCheckpointCommitOrder, "cannot begin global-sequence transaction", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range [][2]any{
		{"global_seq", seq},
		{"config_version", configVersion},
	} {
		key := item[0].(string)
		value := item[1]
		if _, err := tx.Exec(`
INSERT INTO checkpoint_meta(key,value) VALUES(?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return WrapError(CodeCheckpointCommitOrder, "cannot update checkpoint meta", err).
				WithContext("key", key)
		}
	}
	if err := tx.Commit(); err != nil {
		return WrapError(CodeCheckpointCommitOrder, "cannot commit global sequence", err).
			WithContext("seq", seq)
	}
	return nil
}

func (c *Checkpoint) SaveCommit(configVersion string, seq int64, results []WindowResult) *Error {
	return c.SaveCommitBatch(configVersion, seq, results, nil)
}

func (c *Checkpoint) loadMeta() (int64, string, bool, *Error) {
	rows, err := c.db.Query(`SELECT key,value FROM checkpoint_meta WHERE key IN ('global_seq','config_version')`)
	if err != nil {
		return 0, "", false, WrapError(CodeCheckpointTruncated, "cannot read checkpoint meta", err).
			WithContext("path", c.path)
	}
	defer rows.Close()
	var seq int64
	var version string
	var haveSeq, haveVersion bool
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return 0, "", false, WrapError(CodeCheckpointTruncated, "cannot scan checkpoint meta", err)
		}
		if key == "global_seq" {
			_, scanErr := parseInt64(value, &seq)
			if scanErr != nil {
				return 0, "", false, WrapError(CodeCheckpointTruncated, "invalid global_seq", scanErr)
			}
			haveSeq = true
		}
		if key == "config_version" {
			version = value
			haveVersion = true
		}
	}
	return seq, version, haveSeq && haveVersion, nil
}

func parseInt64(s string, target *int64) (int64, error) {
	var v int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not an integer")
		}
		v = v*10 + int64(r-'0')
	}
	*target = v
	return v, nil
}

// Recover reads only complete windows at or below the globally committed
// sequence. Window states written in phase 1 of a crashed commit remain
// orphaned and invisible.
func (c *Checkpoint) Recover(expectedVersion string) (*Recovery, *Error) {
	if err := c.checkIntegrity(); err != nil {
		return nil, err
	}
	seq, version, found, err := c.loadMeta()
	if err != nil {
		return nil, err
	}
	recovery := &Recovery{GlobalSeq: seq, ConfigVersion: version, ExpectedVersion: expectedVersion, Results: map[WindowKey]WindowResult{}}
	if !found {
		return recovery, nil
	}
	if version != expectedVersion {
		coded := NewError(CodeCheckpointVersionMismatch, "checkpoint config version does not match engineering snapshot; full recompute required").
			WithContext("path", c.path).
			WithContext("stored_version", version).
			WithContext("expected_version", expectedVersion).
			WithContext("committed_seq", seq)
		recovery.Found = true
		return recovery, coded
	}

	rows, qerr := c.db.Query(`
SELECT metric_id,start_ms,window_index,result_json
FROM window_states
WHERE config_version=? AND ready=1 AND commit_seq<=?
ORDER BY metric_id,start_ms`, expectedVersion, seq)
	if qerr != nil {
		return nil, WrapError(CodeCheckpointTruncated, "cannot query committed window states", qerr)
	}
	defer rows.Close()
	for rows.Next() {
		var metricID, payload string
		var start, index int64
		if err := rows.Scan(&metricID, &start, &index, &payload); err != nil {
			return nil, WrapError(CodeCheckpointTruncated, "cannot scan window state", err)
		}
		var result WindowResult
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return nil, WrapError(CodeCheckpointTruncated, "window state JSON is truncated", err).
				WithContext("metric_id", metricID).WithContext("start_ms", start)
		}
		recovery.Results[result.Key] = result
	}

	pointRows, perr := c.db.Query(`SELECT point_json FROM samples WHERE seq<=? ORDER BY seq`, seq)
	if perr != nil {
		return nil, WrapError(CodeCheckpointTruncated, "cannot query committed samples", perr)
	}
	for pointRows.Next() {
		var payload string
		if err := pointRows.Scan(&payload); err != nil {
			pointRows.Close()
			return nil, WrapError(CodeCheckpointTruncated, "cannot scan sample", err)
		}
		var point Point
		if err := json.Unmarshal([]byte(payload), &point); err != nil {
			pointRows.Close()
			return nil, WrapError(CodeCheckpointTruncated, "sample JSON is truncated", err)
		}
		recovery.Points = append(recovery.Points, point)
	}
	pointRows.Close()
	if err := pointRows.Err(); err != nil {
		return nil, WrapError(CodeCheckpointTruncated, "sample recovery cursor failed", err)
	}

	orphanRow := c.db.QueryRow(`SELECT COUNT(*) FROM window_states WHERE config_version=? AND commit_seq>?`, expectedVersion, seq)
	if err := orphanRow.Scan(&recovery.OrphanRows); err != nil {
		return nil, WrapError(CodeCheckpointTruncated, "cannot count orphan window states", err)
	}
	recovery.Found = true
	return recovery, nil
}

func (c *Checkpoint) Reset() *Error {
	_, err := c.db.Exec(`DELETE FROM window_states; DELETE FROM samples; DELETE FROM checkpoint_meta;`)
	if err != nil {
		return WrapError(CodeCheckpointCommitOrder, "cannot reset checkpoint for full recompute", err)
	}
	return nil
}
