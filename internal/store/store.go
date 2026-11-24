// Package store persists committed packet windows in SQLite. Every window is
// one transaction; validation failures invalidate the whole snapshot.
package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"audioforge/internal/errs"
	"audioforge/internal/model"

	_ "modernc.org/sqlite"
)

type Snapshot struct {
	JobID             string   `json:"job_id"`
	ParamVersion      string   `json:"param_version"`
	AlgorithmVersion  string   `json:"algorithm_version"`
	CapabilityVersion string   `json:"capability_version"`
	Complete          bool     `json:"complete"`
	Windows           []Window `json:"windows"`
}

type Window struct {
	Index            int            `json:"index"`
	Packets          []model.Packet `json:"packets"`
	ParamVersion     string         `json:"param_version"`
	AlgorithmVersion string         `json:"algorithm_version"`
	CommittedAt      string         `json:"committed_at"`
}

type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errs.New(errs.ESnapshot, "database path is empty")
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, errs.Wrap(errs.ESnapshot, err, "create database directory")
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, errs.Wrap(errs.ESnapshot, err, "open sqlite database")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, errs.Wrap(errs.ESnapshot, err, "configure sqlite connection")
	}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS snapshots (
	job_id TEXT PRIMARY KEY,
	param_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	capability_version TEXT NOT NULL,
	status TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS windows (
	job_id TEXT NOT NULL,
	window_index INTEGER NOT NULL,
	committed_at TEXT NOT NULL,
	PRIMARY KEY (job_id, window_index),
	FOREIGN KEY (job_id) REFERENCES snapshots(job_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS packet_records (
	job_id TEXT NOT NULL,
	channel INTEGER NOT NULL,
	seq INTEGER NOT NULL,
	window_index INTEGER NOT NULL,
	start_sample INTEGER NOT NULL,
	sample_count INTEGER NOT NULL,
	timing_mark INTEGER NOT NULL,
	previous_offset INTEGER NOT NULL,
	samples BLOB NOT NULL,
	depends_events TEXT NOT NULL,
	consumed_offsets TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	param_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	PRIMARY KEY (job_id, channel, seq),
	FOREIGN KEY (job_id) REFERENCES snapshots(job_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_packet_window ON packet_records(job_id, window_index, channel, seq);
`
	if _, err := s.db.Exec(schema); err != nil {
		return errs.Wrap(errs.ESnapshot, err, "create sqlite schema")
	}
	return nil
}

func (s *Store) Begin(jobID, paramVersion, algorithmVersion, capabilityVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if jobID == "" || paramVersion == "" || algorithmVersion == "" {
		return errs.New(errs.ESnapshot, "job, parameter version, and algorithm version are required")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errs.Wrap(errs.ESnapshot, err, "begin snapshot transaction")
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE job_id = ?`, jobID); err != nil {
		return errs.Wrap(errs.ESnapshot, err, "clear prior snapshot")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO snapshots(job_id,param_version,algorithm_version,capability_version,status,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		jobID, paramVersion, algorithmVersion, capabilityVersion, "open", now, now); err != nil {
		return errs.Wrap(errs.ESnapshot, err, "create snapshot")
	}
	if err := tx.Commit(); err != nil {
		return errs.Wrap(errs.ESnapshot, err, "commit snapshot creation")
	}
	return nil
}

func (s *Store) CommitWindow(jobID string, windowIndex int, packets []model.Packet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if windowIndex < 0 {
		return errs.New(errs.ESnapshot, "window index must not be negative")
	}
	if len(packets) == 0 {
		return errs.New(errs.ESnapshot, "cannot commit an empty window")
	}
	ordered := append([]model.Packet(nil), packets...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Channel != ordered[j].Channel {
			return ordered[i].Channel < ordered[j].Channel
		}
		return ordered[i].Seq < ordered[j].Seq
	})
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errs.Wrap(errs.ESnapshot, err, "begin window transaction")
	}
	defer func() { _ = tx.Rollback() }()
	var paramVersion, algorithmVersion, status string
	err = tx.QueryRowContext(ctx,
		`SELECT param_version,algorithm_version,status FROM snapshots WHERE job_id = ?`, jobID).
		Scan(&paramVersion, &algorithmVersion, &status)
	if err != nil {
		if err == sql.ErrNoRows {
			return errs.New(errs.ESnapshot, "snapshot %q does not exist", jobID)
		}
		return errs.Wrap(errs.ESnapshot, err, "read snapshot metadata")
	}
	if status != "open" {
		return errs.New(errs.ESnapshot, "snapshot %q is already finalized", jobID)
	}
	var lastWindow int
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(window_index), -1) FROM windows WHERE job_id = ?`, jobID).Scan(&lastWindow)
	if err != nil {
		return errs.Wrap(errs.ESnapshot, err, "read last committed window")
	}
	if windowIndex != lastWindow+1 {
		return &errs.Error{
			Code:    errs.EState,
			Message: fmt.Sprintf("window %d cannot follow window %d", windowIndex, lastWindow),
			Field:   "window_index",
		}
	}
	type continuityCursor struct {
		loaded bool
		seq    int
		end    int64
	}
	cursors := make(map[int]*continuityCursor)
	for _, packet := range ordered {
		if err := validatePacket(packet, paramVersion, algorithmVersion); err != nil {
			return err
		}
		cursor := cursors[packet.Channel]
		if cursor == nil {
			cursor = &continuityCursor{seq: -1}
			cursors[packet.Channel] = cursor
		}
		if !cursor.loaded {
			var previousSeq, previousCount int
			var previousStart int64
			err := tx.QueryRowContext(ctx,
				`SELECT seq,start_sample,sample_count FROM packet_records
				 WHERE job_id=? AND channel=? ORDER BY seq DESC LIMIT 1`,
				jobID, packet.Channel).Scan(&previousSeq, &previousStart, &previousCount)
			switch {
			case err == sql.ErrNoRows:
			case err != nil:
				return errs.Wrap(errs.ESnapshot, err, "read prior packet continuity")
			default:
				cursor.seq = previousSeq
				cursor.end = previousStart + int64(previousCount)
			}
			cursor.loaded = true
		}
		if packet.Seq != cursor.seq+1 || packet.StartSample != cursor.end {
			return &errs.Error{
				Code:         errs.EPack,
				Message:      fmt.Sprintf("packet %d does not continue sequence %d at sample %d", packet.Seq, cursor.seq, cursor.end),
				PacketSeq:    packet.Seq,
				Channel:      packet.Channel,
				SampleOffset: packet.StartSample,
				Field:        "seq/start_sample",
			}
		}
		cursor.seq = packet.Seq
		cursor.end = packet.EndSample()
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO windows(job_id,window_index,committed_at) VALUES(?,?,?)`,
		jobID, windowIndex, now); err != nil {
		return errs.Wrap(errs.ESnapshot, err, "insert window %d", windowIndex)
	}
	for _, packet := range ordered {
		depends, _ := json.Marshal(packet.DependsOnEvents)
		consumed, _ := json.Marshal(packet.ConsumedOffsets)
		_, err := tx.ExecContext(ctx,
			`INSERT INTO packet_records(
				job_id,channel,seq,window_index,start_sample,sample_count,timing_mark,
				previous_offset,samples,depends_events,consumed_offsets,content_hash,
				param_version,algorithm_version
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			jobID, packet.Channel, packet.Seq, windowIndex, packet.StartSample,
			len(packet.Samples), packet.TimingMark, packet.PreviousOffset,
			encodeSamples(packet.Samples), string(depends), string(consumed),
			packet.ContentHash, paramVersion, algorithmVersion,
		)
		if err != nil {
			return errs.Wrap(errs.ESnapshot, err, "insert packet %d/%d in window %d", packet.Channel, packet.Seq, windowIndex)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshots SET updated_at = ? WHERE job_id = ?`, now, jobID); err != nil {
		return errs.Wrap(errs.ESnapshot, err, "update snapshot timestamp")
	}
	if err := tx.Commit(); err != nil {
		return errs.Wrap(errs.ESnapshot, err, "commit window %d", windowIndex)
	}
	return nil
}

func (s *Store) Finalize(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.Exec(`UPDATE snapshots SET status = 'complete', updated_at = ? WHERE job_id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), jobID)
	if err != nil {
		return errs.Wrap(errs.ESnapshot, err, "finalize snapshot")
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return errs.New(errs.ESnapshot, "snapshot %q does not exist", jobID)
	}
	return nil
}

func (s *Store) Load(jobID, expectedParamVersion, expectedAlgorithmVersion string) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	snapshot := Snapshot{JobID: jobID}
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT param_version,algorithm_version,capability_version,status FROM snapshots WHERE job_id = ?`, jobID).
		Scan(&snapshot.ParamVersion, &snapshot.AlgorithmVersion, &snapshot.CapabilityVersion, &status)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, errs.New(errs.ESnapshot, "snapshot %q does not exist", jobID)
		}
		return nil, errs.Wrap(errs.ESnapshot, err, "load snapshot metadata")
	}
	if snapshot.ParamVersion != expectedParamVersion || snapshot.AlgorithmVersion != expectedAlgorithmVersion {
		_ = s.invalidateLocked(jobID)
		return nil, &errs.Error{
			Code:    errs.ESnapshot,
			Message: "snapshot parameter or algorithm version mismatch; snapshot invalidated",
			Field:   "version",
		}
	}
	snapshot.Complete = status == "complete"
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel,seq,window_index,start_sample,sample_count,timing_mark,previous_offset,
		        samples,depends_events,consumed_offsets,content_hash,param_version,algorithm_version
		 FROM packet_records WHERE job_id = ? ORDER BY window_index,channel,seq`, jobID)
	if err != nil {
		return nil, errs.Wrap(errs.ESnapshot, err, "load packet records")
	}
	defer rows.Close()
	windowByIndex := make(map[int]*Window)
	for rows.Next() {
		var (
			packet       model.Packet
			windowIndex  int
			samples      []byte
			dependsJSON  string
			consumedJSON string
			rowParam     string
			rowAlgorithm string
		)
		if err := rows.Scan(
			&packet.Channel, &packet.Seq, &windowIndex, &packet.StartSample,
			&packet.SampleCount, &packet.TimingMark, &packet.PreviousOffset,
			&samples, &dependsJSON, &consumedJSON, &packet.ContentHash, &rowParam, &rowAlgorithm,
		); err != nil {
			_ = rows.Close()
			_ = s.invalidateLocked(jobID)
			return nil, errs.Wrap(errs.ESnapshot, err, "scan packet record; snapshot invalidated")
		}
		packet.Samples = decodeSamples(samples)
		if err := json.Unmarshal([]byte(dependsJSON), &packet.DependsOnEvents); err != nil {
			_ = rows.Close()
			_ = s.invalidateLocked(jobID)
			return nil, errs.Wrap(errs.ESnapshot, err, "invalid event dependency payload; snapshot invalidated")
		}
		if err := json.Unmarshal([]byte(consumedJSON), &packet.ConsumedOffsets); err != nil {
			_ = rows.Close()
			_ = s.invalidateLocked(jobID)
			return nil, errs.Wrap(errs.ESnapshot, err, "invalid consumed offset payload; snapshot invalidated")
		}
		if len(packet.Samples) != packet.SampleCount ||
			rowParam != snapshot.ParamVersion ||
			rowAlgorithm != snapshot.AlgorithmVersion ||
			model.HashPacket(packet) != packet.ContentHash {
			_ = rows.Close()
			_ = s.invalidateLocked(jobID)
			return nil, &errs.Error{
				Code:      errs.ESnapshot,
				Message:   "packet hash or version mismatch; snapshot invalidated",
				PacketSeq: packet.Seq,
				Channel:   packet.Channel,
				Field:     "content_hash/version",
			}
		}
		window := windowByIndex[windowIndex]
		if window == nil {
			window = &Window{
				Index:            windowIndex,
				ParamVersion:     snapshot.ParamVersion,
				AlgorithmVersion: snapshot.AlgorithmVersion,
			}
			windowByIndex[windowIndex] = window
		}
		window.Packets = append(window.Packets, packet)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		_ = s.invalidateLocked(jobID)
		return nil, errs.Wrap(errs.ESnapshot, err, "iterate packet records; snapshot invalidated")
	}
	windowRows, err := s.db.QueryContext(ctx,
		`SELECT window_index,committed_at FROM windows WHERE job_id = ? ORDER BY window_index`, jobID)
	if err != nil {
		return nil, errs.Wrap(errs.ESnapshot, err, "load window records")
	}
	defer windowRows.Close()
	expectedWindow := 0
	for windowRows.Next() {
		var index int
		var committedAt string
		if err := windowRows.Scan(&index, &committedAt); err != nil {
			_ = windowRows.Close()
			_ = s.invalidateLocked(jobID)
			return nil, errs.Wrap(errs.ESnapshot, err, "scan window record; snapshot invalidated")
		}
		window := windowByIndex[index]
		if index != expectedWindow || window == nil || len(window.Packets) == 0 {
			_ = windowRows.Close()
			_ = s.invalidateLocked(jobID)
			return nil, &errs.Error{Code: errs.ESnapshot, Message: "window sequence or content is invalid; snapshot invalidated", Field: "window_index"}
		}
		window.CommittedAt = committedAt
		snapshot.Windows = append(snapshot.Windows, *window)
		expectedWindow++
	}
	_ = windowRows.Close()
	if err := windowRows.Err(); err != nil {
		_ = s.invalidateLocked(jobID)
		return nil, errs.Wrap(errs.ESnapshot, err, "iterate windows; snapshot invalidated")
	}
	if len(windowByIndex) != len(snapshot.Windows) {
		_ = s.invalidateLocked(jobID)
		return nil, errs.New(errs.ESnapshot, "orphan packet records; snapshot invalidated")
	}
	for _, window := range snapshot.Windows {
		if err := validateWindowContinuity(window.Packets); err != nil {
			_ = s.invalidateLocked(jobID)
			return nil, errs.Wrap(errs.ESnapshot, err, "packet continuity invalid; snapshot invalidated")
		}
	}
	if err := validateSnapshotContinuity(snapshot.Windows); err != nil {
		_ = s.invalidateLocked(jobID)
		return nil, errs.Wrap(errs.ESnapshot, err, "snapshot packet order invalid; snapshot invalidated")
	}
	return &snapshot, nil
}

func (s *Store) Invalidate(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateLocked(jobID)
}

func (s *Store) invalidateLocked(jobID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM packet_records WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM windows WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM snapshots WHERE job_id = ?`, jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Exists(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE job_id = ?`, jobID).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func validatePacket(packet model.Packet, paramVersion, algorithmVersion string) error {
	if packet.ContentHash == "" {
		return errs.New(errs.ESnapshot, "packet content hash is empty")
	}
	if model.HashPacket(packet) != packet.ContentHash {
		return errs.New(errs.ESnapshot, "packet content hash mismatch")
	}
	if len(packet.Samples) != packet.SampleCount {
		return errs.New(errs.ESnapshot, "packet sample count mismatch")
	}
	if paramVersion == "" || algorithmVersion == "" {
		return errs.New(errs.ESnapshot, "missing parameter or algorithm version")
	}
	return nil
}

func validateWindowContinuity(packets []model.Packet) error {
	lastEnd := make(map[int]int64)
	lastSeq := make(map[int]int)
	for _, packet := range packets {
		if end, ok := lastEnd[packet.Channel]; ok && packet.StartSample != end {
			return fmt.Errorf("channel %d packet %d starts at %d, expected %d", packet.Channel, packet.Seq, packet.StartSample, end)
		}
		if seq, ok := lastSeq[packet.Channel]; ok && packet.Seq != seq+1 {
			return fmt.Errorf("channel %d packet sequence jumped from %d to %d", packet.Channel, seq, packet.Seq)
		}
		lastEnd[packet.Channel] = packet.EndSample()
		lastSeq[packet.Channel] = packet.Seq
	}
	return nil
}

func validateSnapshotContinuity(windows []Window) error {
	lastSeq := make(map[int]int)
	lastEnd := make(map[int]int64)
	seenChannel := make(map[int]bool)
	for _, window := range windows {
		for _, packet := range window.Packets {
			if seq, ok := lastSeq[packet.Channel]; ok {
				if packet.Seq != seq+1 {
					return fmt.Errorf("channel %d packet sequence jumped from %d to %d", packet.Channel, seq, packet.Seq)
				}
				if packet.StartSample != lastEnd[packet.Channel] {
					return fmt.Errorf("channel %d packet %d starts at %d, expected %d", packet.Channel, packet.Seq, packet.StartSample, lastEnd[packet.Channel])
				}
			} else {
				if packet.Seq != 0 || packet.StartSample != 0 {
					return fmt.Errorf("channel %d does not begin at sequence 0 and sample 0", packet.Channel)
				}
				seenChannel[packet.Channel] = true
			}
			lastSeq[packet.Channel] = packet.Seq
			lastEnd[packet.Channel] = packet.EndSample()
		}
	}
	_ = seenChannel
	return nil
}

func encodeSamples(samples []int64) []byte {
	out := make([]byte, len(samples)*8)
	for i, sample := range samples {
		binary.BigEndian.PutUint64(out[i*8:], uint64(sample))
	}
	return out
}

func decodeSamples(data []byte) []int64 {
	out := make([]int64, len(data)/8)
	for i := range out {
		out[i] = int64(binary.BigEndian.Uint64(data[i*8:]))
	}
	return out
}
