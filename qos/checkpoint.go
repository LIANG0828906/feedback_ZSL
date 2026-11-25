package qos

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

	_ "modernc.org/sqlite"
)

const checkpointFormat = 1

type CheckpointStore struct {
	db   *sql.DB
	path string
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, NewError(ECheckpointTruncated, "checkpoint.path", "checkpoint path is empty", nil)
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 0 && info.Size() < 100 {
		return nil, checkpointTruncated(path, fmt.Sprintf("file is only %d bytes", info.Size()))
	}
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		return nil, checkpointTruncated(path, err.Error())
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, checkpointTruncated(path, err.Error())
	}
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, checkpointTruncated(path, err.Error())
		}
	}
	var check string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil {
		db.Close()
		return nil, checkpointTruncated(path, err.Error())
	}
	if check != "ok" {
		db.Close()
		return nil, checkpointTruncated(path, check)
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS checkpoint_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  format_version INTEGER NOT NULL,
  project_version TEXT NOT NULL,
  project_digest TEXT NOT NULL,
  next_step INTEGER NOT NULL,
  next_ms INTEGER NOT NULL,
  replay_counter INTEGER NOT NULL,
  emit_counter INTEGER NOT NULL,
  emitted_json TEXT NOT NULL,
  dropped_json TEXT NOT NULL,
  delay_events_json TEXT NOT NULL,
  expired_json TEXT NOT NULL,
  state_checksum TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoint_channels (
  seq INTEGER PRIMARY KEY,
  channel_id TEXT NOT NULL UNIQUE,
  tokens_bytes INTEGER NOT NULL,
  refill_remainder INTEGER NOT NULL,
  weight_credit INTEGER NOT NULL,
  ready_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoint_delays (
  packet_id TEXT PRIMARY KEY,
  expire_at_ms INTEGER NOT NULL,
  deadline_ms INTEGER NOT NULL,
  replay_seq INTEGER NOT NULL,
  reason TEXT NOT NULL
);`); err != nil {
		db.Close()
		return nil, checkpointTruncated(path, err.Error())
	}
	return &CheckpointStore{db: db, path: path}, nil
}

func (s *CheckpointStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *CheckpointStore) Path() string { return s.path }

func (s *CheckpointStore) Save(ctx context.Context, project *Project, snapshot *EngineSnapshot) error {
	if s == nil || s.db == nil {
		return NewError(EInternal, "checkpoint", "store is not open", nil)
	}
	if project == nil || snapshot == nil {
		return NewError(EInternal, "checkpoint", "project and snapshot are required", nil)
	}
	digest, err := project.CanonicalDigest()
	if err != nil {
		return err
	}
	checksum, err := checkpointChecksum(digest, snapshot)
	if err != nil {
		return err
	}
	emittedJSON, err := json.Marshal(snapshot.Emitted)
	if err != nil {
		return wrapError(EInternal, "checkpoint.emitted", err, nil)
	}
	droppedJSON, err := json.Marshal(snapshot.Dropped)
	if err != nil {
		return wrapError(EInternal, "checkpoint.dropped", err, nil)
	}
	delayEventsJSON, err := json.Marshal(snapshot.DelayedEvents)
	if err != nil {
		return wrapError(EInternal, "checkpoint.delay_events", err, nil)
	}
	expiredJSON, err := json.Marshal(snapshot.Expired)
	if err != nil {
		return wrapError(EInternal, "checkpoint.expired", err, nil)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return checkpointWriteError(err)
	}
	defer tx.Rollback()

	// Required write order: channel state, then delay queue state, then the
	// global committed clock/meta row. A kill before commit leaves all three at
	// the previous point; the meta row is the commit marker of record.
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_channels`); err != nil {
		return checkpointWriteError(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_delays`); err != nil {
		return checkpointWriteError(err)
	}
	for i, ch := range snapshot.Channels {
		readyJSON, err := json.Marshal(ch.Ready)
		if err != nil {
			return wrapError(EInternal, "checkpoint.ready", err, nil)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_channels
(seq, channel_id, tokens_bytes, refill_remainder, weight_credit, ready_json)
VALUES (?, ?, ?, ?, ?, ?)`,
			i, ch.ChannelID, ch.TokensBytes, ch.Remainder, ch.Credit, string(readyJSON)); err != nil {
			return checkpointWriteError(err)
		}
	}
	for _, d := range snapshot.Delays {
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_delays
(packet_id, expire_at_ms, deadline_ms, replay_seq, reason) VALUES (?, ?, ?, ?, ?)`,
			d.PacketID, d.ExpireAtMS, d.DeadlineMS, d.ReplaySeq, d.Reason); err != nil {
			return checkpointWriteError(err)
		}
	}
	// This is intentionally the final statement before commit.
	if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoint_meta
(id, format_version, project_version, project_digest, next_step, next_ms,
 replay_counter, emit_counter, emitted_json, dropped_json, delay_events_json,
 expired_json, state_checksum)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 format_version=excluded.format_version,
 project_version=excluded.project_version,
 project_digest=excluded.project_digest,
 next_step=excluded.next_step,
 next_ms=excluded.next_ms,
 replay_counter=excluded.replay_counter,
 emit_counter=excluded.emit_counter,
 emitted_json=excluded.emitted_json,
 dropped_json=excluded.dropped_json,
 delay_events_json=excluded.delay_events_json,
 expired_json=excluded.expired_json,
 state_checksum=excluded.state_checksum`,
		checkpointFormat, project.Version, digest, snapshot.NextStep, snapshot.NextMS,
		snapshot.ReplayCounter, snapshot.EmitCounter, string(emittedJSON), string(droppedJSON),
		string(delayEventsJSON), string(expiredJSON), checksum); err != nil {
		return checkpointWriteError(err)
	}
	if err := tx.Commit(); err != nil {
		return checkpointWriteError(err)
	}
	return nil
}

func checkpointWriteError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewError(EContextCanceled, "checkpoint.save", "checkpoint write canceled", map[string]any{"error": err.Error()})
	}
	return &Error{Code: ECheckpointCorrupt, Path: "checkpoint.save", Message: err.Error(), Err: err}
}

func checkpointTruncated(path, detail string) *Error {
	return NewError(ECheckpointTruncated, "checkpoint", "checkpoint file is truncated or is not a valid SQLite database",
		map[string]any{"path": path, "detail": detail})
}

func (s *CheckpointStore) HasCheckpoint(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, NewError(EInternal, "checkpoint", "store is not open", nil)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoint_meta WHERE id=1`).Scan(&n); err != nil {
		return false, checkpointReadError(err)
	}
	return n > 0, nil
}

func (s *CheckpointStore) Load(ctx context.Context, project *Project) (*EngineSnapshot, error) {
	if s == nil || s.db == nil {
		return nil, NewError(EInternal, "checkpoint", "store is not open", nil)
	}
	if project == nil {
		return nil, NewError(EInternal, "checkpoint", "project is required", nil)
	}
	digest, err := project.CanonicalDigest()
	if err != nil {
		return nil, err
	}
	var (
		formatVersion                 int
		projectVersion, projectDigest string
		nextStep, nextMS              int64
		replayCounter, emitCounter    int64
		emittedJSON, droppedJSON      string
		delayEventsJSON, expiredJSON  string
		checksum                      string
	)
	err = s.db.QueryRowContext(ctx, `SELECT format_version, project_version, project_digest,
next_step, next_ms, replay_counter, emit_counter, emitted_json, dropped_json,
delay_events_json, expired_json, state_checksum
FROM checkpoint_meta WHERE id=1`).Scan(
		&formatVersion, &projectVersion, &projectDigest, &nextStep, &nextMS,
		&replayCounter, &emitCounter, &emittedJSON, &droppedJSON,
		&delayEventsJSON, &expiredJSON, &checksum,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NewError(ECheckpointEmpty, "checkpoint", "no checkpoint has been committed", nil)
	}
	if err != nil {
		return nil, checkpointReadError(err)
	}
	if formatVersion != checkpointFormat {
		return nil, NewError(ECheckpointVersion, "checkpoint.format_version", "checkpoint format version is not supported",
			map[string]any{"checkpoint": formatVersion, "expected": checkpointFormat})
	}
	if projectVersion != project.Version || projectDigest != digest {
		return nil, NewError(ECheckpointVersion, "checkpoint.project",
			"checkpoint project version/digest does not match the requested project",
			map[string]any{
				"checkpoint_version": projectVersion, "project_version": project.Version,
				"checkpoint_digest": projectDigest, "project_digest": digest,
			})
	}
	snapshot := &EngineSnapshot{
		NextStep: int(nextStep), NextMS: nextMS,
		ReplayCounter: replayCounter, EmitCounter: emitCounter,
	}
	if err := json.Unmarshal([]byte(emittedJSON), &snapshot.Emitted); err != nil {
		return nil, checkpointCorrupt("emitted", err)
	}
	if err := json.Unmarshal([]byte(droppedJSON), &snapshot.Dropped); err != nil {
		return nil, checkpointCorrupt("dropped", err)
	}
	if err := json.Unmarshal([]byte(delayEventsJSON), &snapshot.DelayedEvents); err != nil {
		return nil, checkpointCorrupt("delay_events", err)
	}
	if err := json.Unmarshal([]byte(expiredJSON), &snapshot.Expired); err != nil {
		return nil, checkpointCorrupt("expired", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, channel_id, tokens_bytes, refill_remainder,
weight_credit, ready_json FROM checkpoint_channels ORDER BY seq`)
	if err != nil {
		return nil, checkpointReadError(err)
	}
	defer rows.Close()
	expectedSeq := 0
	for rows.Next() {
		var seq int
		var ch ChannelSnapshot
		var readyJSON string
		if err := rows.Scan(&seq, &ch.ChannelID, &ch.TokensBytes, &ch.Remainder, &ch.Credit, &readyJSON); err != nil {
			return nil, checkpointCorrupt("channels", err)
		}
		if seq != expectedSeq {
			return nil, NewError(ECheckpointCorrupt, "checkpoint.channels", "channel sequence is not contiguous", map[string]any{"seq": seq, "expected": expectedSeq})
		}
		expectedSeq++
		if err := json.Unmarshal([]byte(readyJSON), &ch.Ready); err != nil {
			return nil, checkpointCorrupt("ready", err)
		}
		snapshot.Channels = append(snapshot.Channels, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, checkpointReadError(err)
	}
	if err := rows.Close(); err != nil {
		return nil, checkpointReadError(err)
	}
	if len(snapshot.Channels) != len(project.Channels) {
		return nil, NewError(ECheckpointCorrupt, "checkpoint.channels", "channel count mismatch",
			map[string]any{"checkpoint": len(snapshot.Channels), "project": len(project.Channels)})
	}
	drows, err := s.db.QueryContext(ctx, `SELECT packet_id, expire_at_ms, deadline_ms, replay_seq, reason
FROM checkpoint_delays ORDER BY expire_at_ms, packet_id`)
	if err != nil {
		return nil, checkpointReadError(err)
	}
	defer drows.Close()
	for drows.Next() {
		var d DelaySnapshot
		if err := drows.Scan(&d.PacketID, &d.ExpireAtMS, &d.DeadlineMS, &d.ReplaySeq, &d.Reason); err != nil {
			return nil, checkpointCorrupt("delays", err)
		}
		snapshot.Delays = append(snapshot.Delays, d)
	}
	if err := drows.Err(); err != nil {
		return nil, checkpointReadError(err)
	}
	if err := drows.Close(); err != nil {
		return nil, checkpointReadError(err)
	}
	wantChecksum, err := checkpointChecksum(digest, snapshot)
	if err != nil {
		return nil, err
	}
	if checksum != wantChecksum {
		return nil, NewError(ECheckpointCorrupt, "checkpoint.checksum", "checkpoint state checksum mismatch",
			map[string]any{"stored": checksum, "computed": wantChecksum})
	}
	return snapshot, nil
}

func checkpointReadError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "database is locked") {
		return &Error{Code: ECheckpointCorrupt, Path: "checkpoint.read", Message: err.Error(), Err: err}
	}
	if strings.Contains(msg, "malformed") || strings.Contains(msg, "not a database") || strings.Contains(msg, "disk image") {
		return checkpointTruncated("", err.Error())
	}
	return checkpointCorrupt("read", err)
}

func checkpointCorrupt(path string, err error) error {
	return &Error{Code: ECheckpointCorrupt, Path: "checkpoint." + path, Message: err.Error(), Err: err}
}

func checkpointChecksum(projectDigest string, snapshot *EngineSnapshot) (string, error) {
	raw, err := json.Marshal(struct {
		ProjectDigest string
		Snapshot      *EngineSnapshot
	}{ProjectDigest: projectDigest, Snapshot: snapshot})
	if err != nil {
		return "", wrapError(EInternal, "checkpoint.checksum", err, nil)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func ResumeEngine(ctx context.Context, store *CheckpointStore, project *Project) (*Engine, string, error) {
	snap, err := store.Load(ctx, project)
	if err != nil {
		if CodeOf(err) == ECheckpointVersion {
			eng, nerr := NewEngine(project)
			if nerr != nil {
				return nil, "", nerr
			}
			return eng, "full-restart:E_CHECKPOINT_VERSION", err
		}
		if CodeOf(err) == ECheckpointEmpty {
			eng, nerr := NewEngine(project)
			if nerr != nil {
				return nil, "", nerr
			}
			return eng, "fresh:E_CHECKPOINT_EMPTY", nil
		}
		return nil, "", err
	}
	eng, err := RestoreSnapshot(project, snap)
	if err != nil {
		return nil, "", err
	}
	return eng, fmt.Sprintf("resume:step=%d", snap.NextStep), nil
}
