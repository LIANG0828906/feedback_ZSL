package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"edgegateway/internal/model"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Stats struct {
	RTTSamples     []float64
	BacklogPeak    int
	SendWindowPeak int
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL,
  ingress TEXT NOT NULL CHECK (ingress IN ('quic','tcp')),
  connection_id TEXT NOT NULL DEFAULT '',
  active INTEGER NOT NULL DEFAULT 1,
  completed INTEGER NOT NULL DEFAULT 0,
  blocked INTEGER NOT NULL DEFAULT 0,
  blocked_seq INTEGER NOT NULL DEFAULT -1,
  block_reason TEXT NOT NULL DEFAULT '',
  ack_ranges TEXT NOT NULL DEFAULT '[]',
  ack_through INTEGER NOT NULL DEFAULT -1,
  last_seq INTEGER NOT NULL DEFAULT -1,
  migration_count INTEGER NOT NULL DEFAULT 0,
  last_addr TEXT NOT NULL DEFAULT '',
  last_addr_host TEXT NOT NULL DEFAULT '',
  migration_until_unix_ns INTEGER NOT NULL DEFAULT 0,
  rate_limit_per_sec REAL NOT NULL DEFAULT 0,
  limit_active INTEGER NOT NULL DEFAULT 0,
  limit_reason TEXT NOT NULL DEFAULT '',
  limit_basis TEXT NOT NULL DEFAULT '',
  limit_since_unix_ns INTEGER NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0,
  success_count INTEGER NOT NULL DEFAULT 0,
  rejection_count INTEGER NOT NULL DEFAULT 0,
  rtt_samples TEXT NOT NULL DEFAULT '[]',
  backlog_peak INTEGER NOT NULL DEFAULT 0,
  send_window_peak INTEGER NOT NULL DEFAULT 0,
  timeout_count INTEGER NOT NULL DEFAULT 0,
  created_unix_ns INTEGER NOT NULL,
  updated_unix_ns INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS sessions_one_active
  ON sessions(device_id, ingress) WHERE active=1 AND completed=0;
CREATE INDEX IF NOT EXISTS sessions_connection
  ON sessions(ingress, connection_id);
CREATE TABLE IF NOT EXISTS migrations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  fragment_seq INTEGER NOT NULL DEFAULT -1,
  from_addr TEXT NOT NULL,
  to_addr TEXT NOT NULL,
  at_unix_ns INTEGER NOT NULL,
  ack_ranges TEXT NOT NULL,
  ack_through INTEGER NOT NULL,
  preserved_limited INTEGER NOT NULL,
  preserved_rate REAL NOT NULL,
  preserved_reason TEXT NOT NULL,
  trigger TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS migrations_session ON migrations(session_id, id);
CREATE TABLE IF NOT EXISTS feedback_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  ingress TEXT NOT NULL,
  at_unix_ns INTEGER NOT NULL,
  source TEXT NOT NULL,
  basis TEXT NOT NULL,
  reason TEXT NOT NULL,
  backlog INTEGER NOT NULL,
  rtt_median_ms REAL NOT NULL,
  prior_rate REAL NOT NULL,
  new_rate REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS feedback_session ON feedback_events(session_id, id);
CREATE TABLE IF NOT EXISTS rejections (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  fragment_seq INTEGER NOT NULL,
  reason TEXT NOT NULL,
  backend_code TEXT NOT NULL,
  at_unix_ns INTEGER NOT NULL,
  recovered_at_unix_ns INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS rejections_session ON rejections(session_id, fragment_seq);
CREATE TABLE IF NOT EXISTS fragments (
  session_id TEXT NOT NULL REFERENCES sessions(id),
  seq INTEGER NOT NULL,
  state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  checksum TEXT NOT NULL DEFAULT '',
  updated_unix_ns INTEGER NOT NULL,
  PRIMARY KEY(session_id, seq)
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	_, _ = s.db.Exec(`ALTER TABLE sessions ADD COLUMN send_window_peak INTEGER NOT NULL DEFAULT 0`)
	return nil
}

func (s *Store) Secret(ctx context.Context) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='resume_secret'`).Scan(&b)
	if err == nil && len(b) == 32 {
		return b, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('resume_secret',?)`, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func scanSession(scanner interface{ Scan(...any) error }) (*model.Session, error) {
	var s model.Session
	var active, completed, blocked, limited int
	var ackRanges, rttSamples string
	err := scanner.Scan(
		&s.ID, &s.DeviceID, &s.Ingress, &s.ConnectionID,
		&active, &completed, &blocked, &s.BlockedSeq, &s.BlockReason,
		&ackRanges, &s.AckThrough, &s.LastSeq, &s.MigrationCount,
		&s.LastAddr, &s.LastAddrHost, &s.MigrationUntilUnix,
		&s.RateLimitPerSec, &limited, &s.LimitReason, &s.LimitBasis, &s.LimitSinceUnix,
		&s.FailureCount, &s.SuccessCount, &s.RejectionCount,
		&rttSamples, &s.BacklogPeak, &s.SendWindowPeak, &s.TimeoutCount,
		&s.CreatedUnix, &s.UpdatedUnix,
	)
	if err != nil {
		return nil, err
	}
	s.Active = active != 0
	s.Completed = completed != 0
	s.Blocked = blocked != 0
	s.LimitActive = limited != 0
	s.AckRanges = model.DecodeRanges(ackRanges)
	if s.RTTSamplesMS == nil {
		_ = decodeFloatSlice(rttSamples, &s.RTTSamplesMS)
	}
	if len(s.RTTSamplesMS) > 0 {
		s.RTTMedianMS = median(s.RTTSamplesMS)
	}
	return &s, nil
}

const sessionCols = `id,device_id,ingress,connection_id,active,completed,blocked,
blocked_seq,block_reason,ack_ranges,ack_through,last_seq,migration_count,
last_addr,last_addr_host,migration_until_unix_ns,rate_limit_per_sec,
limit_active,limit_reason,limit_basis,limit_since_unix_ns,failure_count,
	success_count,rejection_count,rtt_samples,backlog_peak,send_window_peak,timeout_count,
created_unix_ns,updated_unix_ns`

func getSessionTx(ctx context.Context, tx *sql.Tx, id string) (*model.Session, error) {
	return scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id=?`, id))
}

func (s *Store) GetSession(ctx context.Context, id string) (*model.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id=?`, id))
}

func (s *Store) GetActiveByDevice(ctx context.Context, deviceID, ingress string) (*model.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions
		 WHERE device_id=? AND ingress=? AND active=1 AND completed=0`, deviceID, ingress))
}

func (s *Store) GetByConnection(ctx context.Context, deviceID, ingress, connectionID string) (*model.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions
		 WHERE device_id=? AND ingress=? AND connection_id=? AND active=1 AND completed=0`,
		deviceID, ingress, connectionID))
}

// CreateOrGetSession returns the single active session for a device/ingress.
// A duplicate establish request therefore returns the original reference.
func (s *Store) CreateOrGetSession(ctx context.Context, deviceID, ingress, connectionID, addr, addrHost string, maxSessions int) (*model.Session, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	existing, err := scanSession(tx.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions
		 WHERE device_id=? AND ingress=? AND active=1 AND completed=0`, deviceID, ingress))
	if err == nil {
		if existing.ConnectionID == "" && connectionID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET connection_id=? WHERE id=?`, connectionID, existing.ID); err != nil {
				return nil, false, err
			}
			existing.ConnectionID = connectionID
		}
		return existing, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE active=1 AND completed=0`).Scan(&active); err != nil {
		return nil, false, err
	}
	if active >= maxSessions {
		return nil, false, ErrSessionLimit
	}
	now := time.Now().UnixNano()
	id := newID("ses_")
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(
		id,device_id,ingress,connection_id,active,completed,blocked,blocked_seq,
		block_reason,ack_ranges,ack_through,last_seq,migration_count,last_addr,
		last_addr_host,migration_until_unix_ns,rate_limit_per_sec,limit_active,
		limit_reason,limit_basis,limit_since_unix_ns,failure_count,success_count,
		rejection_count,rtt_samples,backlog_peak,send_window_peak,timeout_count,created_unix_ns,updated_unix_ns
	) VALUES(?,?,?,?,1,0,0,-1,'','[]',-1,-1,0,?,?,0,0,0,'','',0,0,0,0,'[]',0,0,0,?,?)`,
		id, deviceID, ingress, connectionID, addr, addrHost, now, now)
	if err != nil {
		return nil, false, err
	}
	out, err := getSessionTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return out, false, nil
}

var ErrSessionLimit = errors.New("session limit reached")

func normalizedHost(addr string) string {
	if addr == "" {
		return ""
	}
	host := addr
	if i := strings.LastIndex(host, ":"); i > 0 {
		// Keep IPv6 host portions intact when they arrive as [::1]:port.
		if strings.HasPrefix(host, "[") {
			if j := strings.LastIndex(host, "]"); j > 0 {
				host = host[1:j]
			}
		} else {
			host = host[:i]
		}
	}
	return strings.ToLower(host)
}

func (s *Store) ObserveAddress(ctx context.Context, id, addr, host string, countMigration bool, window time.Duration, seq int64, trigger string) (*model.Session, *model.Migration, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	sess, err := getSessionTx(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	if host == "" {
		host = normalizedHost(addr)
	}
	oldHost := sess.LastAddrHost
	if oldHost == "" && sess.LastAddr != "" {
		oldHost = normalizedHost(sess.LastAddr)
	}
	migrated := countMigration && oldHost != "" && host != "" && oldHost != host
	now := time.Now().UnixNano()
	if !migrated {
		if addr != "" && addr != sess.LastAddr {
			_, err = tx.ExecContext(ctx, `UPDATE sessions SET last_addr=?,last_addr_host=?,updated_unix_ns=? WHERE id=?`,
				addr, host, now, sess.ID)
			if err != nil {
				return nil, nil, err
			}
			sess.LastAddr, sess.LastAddrHost, sess.UpdatedUnix = addr, host, now
		}
		return sess, nil, tx.Commit()
	}
	from := sess.LastAddr
	mig := &model.Migration{
		SessionID:        sess.ID,
		Seq:              seq,
		FromAddr:         from,
		ToAddr:           addr,
		AtUnix:           now,
		AckRanges:        append([]model.Range(nil), sess.AckRanges...),
		AckThrough:       sess.AckThrough,
		PreservedLimited: sess.LimitActive,
		PreservedRate:    sess.RateLimitPerSec,
		PreservedReason:  sess.LimitReason,
		Trigger:          trigger,
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO migrations(
		session_id,fragment_seq,from_addr,to_addr,at_unix_ns,ack_ranges,ack_through,
		preserved_limited,preserved_rate,preserved_reason,trigger
	) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, seq, from, addr, now, model.EncodeRanges(sess.AckRanges), sess.AckThrough,
		boolInt(sess.LimitActive), sess.RateLimitPerSec, sess.LimitReason, trigger)
	if err != nil {
		return nil, nil, err
	}
	mig.ID, _ = res.LastInsertId()
	until := now + window.Nanoseconds()
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET
		migration_count=migration_count+1,last_addr=?,last_addr_host=?,
		migration_until_unix_ns=?,updated_unix_ns=? WHERE id=?`,
		addr, host, until, now, sess.ID)
	if err != nil {
		return nil, nil, err
	}
	sess.MigrationCount++
	sess.LastAddr, sess.LastAddrHost = addr, host
	sess.MigrationUntilUnix, sess.UpdatedUnix = until, now
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return sess, mig, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// RecordAccepted commits the accepted fragment, merged acknowledgement range,
// session counters, and recovery of a previously rejected sequence together.
func (s *Store) RecordAccepted(ctx context.Context, sessionID string, seq int64, size int64, checksum string, backendRanges []model.Range, stats Stats) (*model.Session, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	sess, err := getSessionTx(ctx, tx, sessionID)
	if err != nil {
		return nil, false, err
	}
	var state string
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT state,attempts FROM fragments WHERE session_id=? AND seq=?`, sessionID, seq).Scan(&state, &attempts)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	alreadyAccepted := err == nil && state == "accepted"
	if alreadyAccepted || model.Contains(sess.AckRanges, seq) {
		return sess, true, tx.Commit()
	}
	now := time.Now().UnixNano()
	merged := model.MergeRanges(sess.AckRanges, backendRanges)
	merged = model.MergeRange(merged, seq)
	_, err = tx.ExecContext(ctx, `INSERT INTO fragments(session_id,seq,state,attempts,reason,size,checksum,updated_unix_ns)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id,seq) DO UPDATE SET
		state='accepted',attempts=fragments.attempts+1,reason='',size=excluded.size,
		checksum=excluded.checksum,updated_unix_ns=excluded.updated_unix_ns`,
		sessionID, seq, "accepted", 1, "", size, checksum, now)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rejections SET recovered_at_unix_ns=? WHERE session_id=? AND fragment_seq=? AND recovered_at_unix_ns=0`,
		now, sessionID, seq); err != nil {
		return nil, false, err
	}
	blocked, blockSeq, blockReason := sess.Blocked, sess.BlockedSeq, sess.BlockReason
	if blocked && blockSeq == seq {
		blocked, blockSeq, blockReason = false, -1, ""
	}
	lastSeq := sess.LastSeq
	if seq > lastSeq {
		lastSeq = seq
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET
		ack_ranges=?,ack_through=?,last_seq=?,blocked=?,blocked_seq=?,block_reason=?,
		success_count=success_count+1,rtt_samples=?,backlog_peak=?,send_window_peak=?,updated_unix_ns=? WHERE id=?`,
		model.EncodeRanges(merged), model.AckThrough(merged), lastSeq, boolInt(blocked), blockSeq,
		blockReason, encodeFloatSlice(stats.RTTSamples), maxInt(sess.BacklogPeak, stats.BacklogPeak),
		maxInt(sess.SendWindowPeak, stats.SendWindowPeak), now, sessionID)
	if err != nil {
		return nil, false, err
	}
	sess.AckRanges, sess.AckThrough, sess.LastSeq = merged, model.AckThrough(merged), lastSeq
	sess.Blocked, sess.BlockedSeq, sess.BlockReason = blocked, blockSeq, blockReason
	sess.SuccessCount++
	sess.RTTSamplesMS = append([]float64(nil), stats.RTTSamples...)
	if len(sess.RTTSamplesMS) > 0 {
		sess.RTTMedianMS = median(sess.RTTSamplesMS)
	}
	sess.BacklogPeak = maxInt(sess.BacklogPeak, stats.BacklogPeak)
	sess.SendWindowPeak = maxInt(sess.SendWindowPeak, stats.SendWindowPeak)
	sess.UpdatedUnix = now
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return sess, false, nil
}

func (s *Store) RecordRejected(ctx context.Context, sessionID string, seq int64, size int64, checksum, reason, backendCode string, stats Stats) (*model.Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	sess, err := getSessionTx(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixNano()
	_, err = tx.ExecContext(ctx, `INSERT INTO fragments(session_id,seq,state,attempts,reason,size,checksum,updated_unix_ns)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id,seq) DO UPDATE SET state='rejected',attempts=fragments.attempts+1,
		reason=excluded.reason,size=excluded.size,checksum=excluded.checksum,updated_unix_ns=excluded.updated_unix_ns`,
		sessionID, seq, "rejected", 1, reason, size, checksum, now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rejections(session_id,fragment_seq,reason,backend_code,at_unix_ns)
		VALUES(?,?,?,?,?)`, sessionID, seq, reason, backendCode, now); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET
		blocked=1,blocked_seq=?,block_reason=?,failure_count=failure_count+1,
		rejection_count=rejection_count+1,rtt_samples=?,backlog_peak=?,send_window_peak=?,updated_unix_ns=?
		WHERE id=?`,
		seq, reason, encodeFloatSlice(stats.RTTSamples), maxInt(sess.BacklogPeak, stats.BacklogPeak),
		maxInt(sess.SendWindowPeak, stats.SendWindowPeak), now, sessionID)
	if err != nil {
		return nil, err
	}
	sess.Blocked, sess.BlockedSeq, sess.BlockReason = true, seq, reason
	sess.FailureCount++
	sess.RejectionCount++
	sess.RTTSamplesMS = append([]float64(nil), stats.RTTSamples...)
	if len(sess.RTTSamplesMS) > 0 {
		sess.RTTMedianMS = median(sess.RTTSamplesMS)
	}
	sess.BacklogPeak = maxInt(sess.BacklogPeak, stats.BacklogPeak)
	sess.SendWindowPeak = maxInt(sess.SendWindowPeak, stats.SendWindowPeak)
	sess.UpdatedUnix = now
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Store) RecordTimeout(ctx context.Context, sessionID string, seq int64, size int64, checksum, reason string, stats Stats) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sess, err := getSessionTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `INSERT INTO fragments(session_id,seq,state,attempts,reason,size,checksum,updated_unix_ns)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id,seq) DO UPDATE SET state='error',attempts=fragments.attempts+1,
		reason=excluded.reason,updated_unix_ns=excluded.updated_unix_ns`,
		sessionID, seq, "error", 1, reason, size, checksum, now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET timeout_count=timeout_count+1,
		rtt_samples=?,backlog_peak=?,send_window_peak=?,updated_unix_ns=? WHERE id=?`,
		encodeFloatSlice(stats.RTTSamples), maxInt(sess.BacklogPeak, stats.BacklogPeak),
		maxInt(sess.SendWindowPeak, stats.SendWindowPeak), now, sessionID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordFeedback(ctx context.Context, sess *model.Session, active bool, reason, basis string, rate float64, backlog int, rttMedian float64) (*model.FeedbackEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	prior := sess.RateLimitPerSec
	if !sess.LimitActive {
		prior = 0
	}
	event := &model.FeedbackEvent{
		SessionID: sess.ID, Ingress: sess.Ingress, AtUnix: now,
		Source: "backend_backpressure", Basis: basis, Reason: reason,
		Backlog: backlog, RTTMedianMS: rttMedian, PriorRate: prior, NewRate: rate,
	}
	if !active {
		event.Source = "backend_feedback_recovered"
		event.Reason = "thresholds_clear"
		event.NewRate = 0
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO feedback_events(
		session_id,ingress,at_unix_ns,source,basis,reason,backlog,rtt_median_ms,prior_rate,new_rate
	) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		sess.ID, sess.Ingress, now, event.Source, basis, event.Reason, backlog, rttMedian, prior, event.NewRate)
	if err != nil {
		return nil, err
	}
	event.ID, _ = res.LastInsertId()
	limitActive := 0
	limitSince := now
	if active {
		limitActive = 1
	} else {
		limitSince = 0
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET rate_limit_per_sec=?,limit_active=?,
		limit_reason=?,limit_basis=?,limit_since_unix_ns=?,updated_unix_ns=? WHERE id=?`,
		event.NewRate, limitActive, event.Reason, basis, limitSince, now, sess.ID)
	if err != nil {
		return nil, err
	}
	sess.RateLimitPerSec = event.NewRate
	sess.LimitActive = active
	sess.LimitReason = event.Reason
	sess.LimitBasis = basis
	sess.LimitSinceUnix = limitSince
	sess.UpdatedUnix = now
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return event, nil
}

func (s *Store) CompleteSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET active=0,completed=1,updated_unix_ns=? WHERE id=? AND active=1`,
		time.Now().UnixNano(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListSessions(ctx context.Context, activeOnly bool, limit int) ([]*model.Session, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT ` + sessionCols + ` FROM sessions`
	if activeOnly {
		q += ` WHERE active=1 AND completed=0`
	}
	q += ` ORDER BY created_unix_ns DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) Migrations(ctx context.Context, sessionID string) ([]model.Migration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,fragment_seq,from_addr,to_addr,at_unix_ns,
		ack_ranges,ack_through,preserved_limited,preserved_rate,preserved_reason,trigger
		FROM migrations WHERE session_id=? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Migration
	for rows.Next() {
		var m model.Migration
		var ar string
		var limited int
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Seq, &m.FromAddr, &m.ToAddr, &m.AtUnix,
			&ar, &m.AckThrough, &limited, &m.PreservedRate, &m.PreservedReason, &m.Trigger); err != nil {
			return nil, err
		}
		m.AckRanges = model.DecodeRanges(ar)
		m.PreservedLimited = limited != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) Feedback(ctx context.Context, sessionID string, limit int) ([]model.FeedbackEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,ingress,at_unix_ns,source,basis,reason,
		backlog,rtt_median_ms,prior_rate,new_rate FROM feedback_events WHERE session_id=? ORDER BY id DESC LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FeedbackEvent
	for rows.Next() {
		var e model.FeedbackEvent
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Ingress, &e.AtUnix, &e.Source, &e.Basis,
			&e.Reason, &e.Backlog, &e.RTTMedianMS, &e.PriorRate, &e.NewRate); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Rejections(ctx context.Context, sessionID string, limit int) ([]model.Rejection, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,fragment_seq,reason,backend_code,
		at_unix_ns,recovered_at_unix_ns FROM rejections WHERE session_id=? ORDER BY id DESC LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Rejection
	for rows.Next() {
		var r model.Rejection
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Seq, &r.Reason, &r.BackendCode,
			&r.AtUnix, &r.RecoveredAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Fragments(ctx context.Context, sessionID string, limit int) ([]model.FragmentState, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,state,attempts,reason,size,checksum,updated_unix_ns
		FROM fragments WHERE session_id=? ORDER BY seq LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FragmentState
	for rows.Next() {
		var f model.FragmentState
		if err := rows.Scan(&f.Seq, &f.State, &f.Attempts, &f.Reason, &f.Size, &f.Checksum, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) CountActive(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ingress,COUNT(*) FROM sessions WHERE active=1 AND completed=0 GROUP BY ingress`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{"quic": 0, "tcp": 0}
	for rows.Next() {
		var ingress string
		var n int
		if err := rows.Scan(&ingress, &n); err != nil {
			return nil, err
		}
		out[ingress] = n
	}
	return out, rows.Err()
}

func (s *Store) ClearBlock(ctx context.Context, sessionID string) (*model.Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	sess, err := getSessionTx(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	if !sess.Blocked {
		return sess, tx.Commit()
	}
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET blocked=0,blocked_seq=-1,block_reason='',updated_unix_ns=? WHERE id=?`, now, sessionID); err != nil {
		return nil, err
	}
	sess.Blocked, sess.BlockedSeq, sess.BlockReason, sess.UpdatedUnix = false, -1, "", now
	return sess, tx.Commit()
}

func encodeFloatSlice(v []float64) string {
	if v == nil {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeFloatSlice(s string, out *[]float64) error {
	return json.Unmarshal([]byte(s), out)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	cp := append([]float64(nil), v...)
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
