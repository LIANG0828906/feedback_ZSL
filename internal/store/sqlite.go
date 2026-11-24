package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"levelacoustic/internal/model"
)

type CheckpointRow struct {
	RoomID           string
	Revision         string
	Payload          []byte
	ContentHash      string
	ParameterVersion string
	AlgorithmVersion string
	CommitOrder      int
	UpdatedUnixNano  int64
}

type StoreStats struct {
	Writes           int64  `json:"writes"`
	QueueWaits       int64  `json:"queue_waits"`
	TotalQueueWaitMs int64  `json:"total_queue_wait_ms"`
	LastRoomID       string `json:"last_room_id,omitempty"`
}

type writeRequest struct {
	ctx  context.Context
	fn   func(*sql.Tx) error
	resp chan error
}

type Store struct {
	path     string
	db       *sql.DB
	requests chan writeRequest
	done     chan struct{}
	closeMu  sync.Mutex
	closed   bool
	writes   atomic.Int64
	waits    atomic.Int64
	waitMS   atomic.Int64
	lastRoom atomic.Value
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, model.NewError(model.ErrIO, "sqlite path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create sqlite directory: %w", err)
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{
		path:     path,
		db:       db,
		requests: make(chan writeRequest, 64),
		done:     make(chan struct{}),
	}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	go s.writerLoop()
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) initSchema() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS room_checkpoints (
			room_id TEXT PRIMARY KEY,
			revision TEXT NOT NULL,
			payload BLOB NOT NULL,
			content_hash TEXT NOT NULL,
			parameter_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			commit_order INTEGER NOT NULL,
			updated_unix_nano INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_log (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			room_id TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			commit_order INTEGER NOT NULL,
			committed_unix_nano INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS app_meta (
			key TEXT PRIMARY KEY,
			value BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_checkpoint_log_room ON checkpoint_log(room_id)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writerLoop() {
	defer close(s.done)
	for req := range s.requests {
		tx, err := s.db.BeginTx(req.ctx, &sql.TxOptions{})
		if err != nil {
			req.resp <- err
			continue
		}
		if err := req.fn(tx); err != nil {
			_ = tx.Rollback()
			req.resp <- err
			continue
		}
		if err := tx.Commit(); err != nil {
			req.resp <- err
			continue
		}
		req.resp <- nil
	}
}

func (s *Store) serialWrite(ctx context.Context, fn func(*sql.Tx) error) error {
	start := time.Now()
	req := writeRequest{ctx: ctx, fn: fn, resp: make(chan error, 1)}
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return model.NewError(model.ErrCheckpoint, "checkpoint store is closed")
	}
	select {
	case s.requests <- req:
		s.closeMu.Unlock()
		wait := time.Since(start)
		if wait > time.Millisecond {
			s.waits.Add(1)
			s.waitMS.Add(wait.Milliseconds())
		}
	case <-ctx.Done():
		s.closeMu.Unlock()
		return ctx.Err()
	}
	select {
	case err := <-req.resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) CommitRoom(ctx context.Context, cp model.RoomCheckpoint) error {
	if cp.RoomID == "" {
		return model.NewError(model.ErrCheckpoint, "checkpoint room id is empty")
	}
	if !cp.VerifyHash() {
		return model.NewError(model.ErrCheckpoint, "room %s checkpoint content hash mismatch before write", cp.RoomID)
	}
	payload, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	now := time.Now().UnixNano()
	err = s.serialWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO room_checkpoints
			(room_id, revision, payload, content_hash, parameter_version, algorithm_version, commit_order, updated_unix_nano)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(room_id) DO UPDATE SET
				revision=excluded.revision,
				payload=excluded.payload,
				content_hash=excluded.content_hash,
				parameter_version=excluded.parameter_version,
				algorithm_version=excluded.algorithm_version,
				commit_order=excluded.commit_order,
				updated_unix_nano=excluded.updated_unix_nano`,
			cp.RoomID, cp.Revision, payload, cp.ContentHash, cp.ParameterVersion, cp.AlgorithmVersion, cp.CommitOrder, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO checkpoint_log
			(room_id, content_hash, commit_order, committed_unix_nano) VALUES (?, ?, ?, ?)`,
			cp.RoomID, cp.ContentHash, cp.CommitOrder, now)
		return err
	})
	if err == nil {
		s.writes.Add(1)
		s.lastRoom.Store(cp.RoomID)
	}
	return err
}

func (s *Store) LoadAll(ctx context.Context) ([]CheckpointRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT room_id, revision, payload, content_hash,
		parameter_version, algorithm_version, commit_order, updated_unix_nano
		FROM room_checkpoints`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckpointRow
	for rows.Next() {
		var row CheckpointRow
		if err := rows.Scan(&row.RoomID, &row.Revision, &row.Payload, &row.ContentHash,
			&row.ParameterVersion, &row.AlgorithmVersion, &row.CommitOrder, &row.UpdatedUnixNano); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CommitOrder != out[j].CommitOrder {
			return out[i].CommitOrder < out[j].CommitOrder
		}
		return out[i].RoomID < out[j].RoomID
	})
	return out, nil
}

func (s *Store) PurgeAll(ctx context.Context) error {
	return s.serialWrite(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM room_checkpoints`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint_log`); err != nil {
			return err
		}
		return nil
	})
}

func (s *Store) PutMeta(ctx context.Context, key string, value []byte) error {
	if key == "" {
		return model.NewError(model.ErrCheckpoint, "meta key is empty")
	}
	return s.serialWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO app_meta(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
		return err
	})
}

func (s *Store) GetMeta(ctx context.Context, key string) ([]byte, bool, error) {
	var value []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (s *Store) CheckpointLog(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, room_id, content_hash, commit_order, committed_unix_nano
		FROM checkpoint_log ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var seq, commitOrder, ts int64
		var roomID, hash string
		if err := rows.Scan(&seq, &roomID, &hash, &commitOrder, &ts); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"seq": seq, "room_id": roomID, "content_hash": hash,
			"commit_order": commitOrder, "committed_unix_nano": ts,
		})
	}
	return out, rows.Err()
}

func (s *Store) Stats() StoreStats {
	last, _ := s.lastRoom.Load().(string)
	return StoreStats{
		Writes:           s.writes.Load(),
		QueueWaits:       s.waits.Load(),
		TotalQueueWaitMs: s.waitMS.Load(),
		LastRoomID:       last,
	}
}

func (s *Store) Close() error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	close(s.requests)
	s.closeMu.Unlock()
	<-s.done
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.db.Close()
}
