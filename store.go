package queryopt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

const (
	queryStateDurable       = "STATE_DURABLE"
	checkpointFormatVersion = 1
)

var ErrCrashSimulated = errors.New("simulated process kill after query state commit")

type Store struct {
	db   *sql.DB
	path string
}

type CommittedQuery struct {
	Ordinal       int             `json:"ordinal"`
	QueryID       string          `json:"query_id"`
	Status        string          `json:"status"`
	SelectedOrder []string        `json:"selected_order"`
	Plan          *QueryPlan      `json:"plan"`
	Estimates     *EstimateReport `json:"estimates"`
	Checksum      string          `json:"checksum"`
}

func OpenStore(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, newError(CodeStoreFailed, "open sqlite: %v", err).with("path", path)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init(ctx context.Context) error {
	ddl := `
CREATE TABLE IF NOT EXISTS query_state (
  ordinal INTEGER PRIMARY KEY,
  query_id TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL,
  selected_order_json TEXT NOT NULL,
  plan_json TEXT NOT NULL,
  estimates_json TEXT NOT NULL,
  checksum TEXT NOT NULL,
  created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
CREATE TABLE IF NOT EXISTS global_cursor (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  committed_ordinal INTEGER NOT NULL
);
INSERT OR IGNORE INTO global_cursor(id, committed_ordinal) VALUES (1, 0);
CREATE TABLE IF NOT EXISTS checkpoints (
  kind TEXT PRIMARY KEY,
  format_version INTEGER NOT NULL,
  engineering_version TEXT NOT NULL,
  payload BLOB NOT NULL,
  payload_len INTEGER NOT NULL,
  checksum TEXT NOT NULL,
  created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);`
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return newError(CodeStoreFailed, "initialize sqlite schema: %v", err)
	}
	return nil
}

func checksumBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CommitQuery persists query state first. The global ordinal watermark is only
// advanced afterwards, so readers never observe an unfinished future query.
func (s *Store) CommitQuery(ctx context.Context, rec CommittedQuery) error {
	return s.commitQuery(ctx, rec, false)
}

// CommitQueryWithCrashAfterState models kill -9 immediately after the first
// transaction commits and before the global cursor transaction starts.
func (s *Store) CommitQueryWithCrashAfterState(ctx context.Context, rec CommittedQuery) error {
	return s.commitQuery(ctx, rec, true)
}

func (s *Store) commitQuery(ctx context.Context, rec CommittedQuery, crashAfterState bool) error {
	if rec.Plan == nil {
		return newError(CodeInputInvalid, "commit requires plan").with("query_id", rec.QueryID)
	}
	planJSON, err := json.Marshal(rec.Plan)
	if err != nil {
		return newError(CodeInputInvalid, "marshal plan: %v", err)
	}
	estJSON := []byte(`{"predicates":{}}`)
	if rec.Estimates != nil {
		estJSON, err = json.Marshal(rec.Estimates)
		if err != nil {
			return newError(CodeInputInvalid, "marshal estimates: %v", err)
		}
	}
	orderJSON, err := json.Marshal(rec.Plan.SelectedOrder)
	if err != nil {
		return newError(CodeInputInvalid, "marshal selected order: %v", err)
	}
	checksum := checksumBytes(planJSON, estJSON)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return newError(CodeStoreFailed, "begin query state tx: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO query_state(ordinal, query_id, status, selected_order_json, plan_json, estimates_json, checksum)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(ordinal) DO UPDATE SET
  query_id=excluded.query_id,
  status=excluded.status,
  selected_order_json=excluded.selected_order_json,
  plan_json=excluded.plan_json,
  estimates_json=excluded.estimates_json,
  checksum=excluded.checksum`,
		rec.Ordinal, rec.QueryID, queryStateDurable, string(orderJSON), string(planJSON), string(estJSON), checksum)
	if err != nil {
		_ = tx.Rollback()
		return newError(CodeStoreFailed, "write query state: %v", err).with("query_id", rec.QueryID)
	}
	if err := tx.Commit(); err != nil {
		return newError(CodeStoreFailed, "commit query state: %v", err).with("query_id", rec.QueryID)
	}
	if crashAfterState {
		_ = s.db.Close()
		return ErrCrashSimulated
	}
	return s.advanceCursor(ctx, rec.Ordinal)
}

func (s *Store) advanceCursor(ctx context.Context, ordinal int) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE global_cursor
SET committed_ordinal = ?
WHERE id = 1 AND committed_ordinal < ?`, ordinal, ordinal)
	if err != nil {
		return newError(CodeStoreFailed, "advance global ordinal: %v", err).with("ordinal", ordinal)
	}
	return nil
}

func (s *Store) GlobalCommittedOrdinal(ctx context.Context) (int, error) {
	var v int
	if err := s.db.QueryRowContext(ctx, `SELECT committed_ordinal FROM global_cursor WHERE id=1`).Scan(&v); err != nil {
		return 0, newError(CodeStoreFailed, "read global cursor: %v", err)
	}
	return v, nil
}

// Recover advances the watermark over contiguous, checksum-valid durable
// query-state rows that survived a crash between state and cursor commit.
func (s *Store) Recover(ctx context.Context) ([]int, error) {
	cursor, err := s.GlobalCommittedOrdinal(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT ordinal, query_id, plan_json, estimates_json, checksum
FROM query_state
WHERE ordinal > ?
ORDER BY ordinal ASC`, cursor)
	if err != nil {
		return nil, newError(CodeStoreFailed, "scan recovery rows: %v", err)
	}
	type recoveryRow struct {
		ordinal                          int
		qid, planJSON, estJSON, checksum string
	}
	var pending []recoveryRow
	var recovered []int
	for rows.Next() {
		var row recoveryRow
		if err := rows.Scan(&row.ordinal, &row.qid, &row.planJSON, &row.estJSON, &row.checksum); err != nil {
			_ = rows.Close()
			return recovered, newError(CodeStoreFailed, "scan recovery row: %v", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return recovered, newError(CodeStoreFailed, "iterate recovery rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		return recovered, newError(CodeStoreFailed, "close recovery rows: %v", err)
	}
	for _, row := range pending {
		if row.ordinal != cursor+1 {
			break
		}
		if checksumBytes([]byte(row.planJSON), []byte(row.estJSON)) != row.checksum {
			break
		}
		var plan QueryPlan
		if err := json.Unmarshal([]byte(row.planJSON), &plan); err != nil {
			break
		}
		var est EstimateReport
		if err := json.Unmarshal([]byte(row.estJSON), &est); err != nil {
			break
		}
		if err := s.advanceCursor(ctx, row.ordinal); err != nil {
			return recovered, err
		}
		cursor = row.ordinal
		recovered = append(recovered, row.ordinal)
	}
	return recovered, nil
}

func (s *Store) LoadCommitted(ctx context.Context) ([]CommittedQuery, error) {
	cursor, err := s.GlobalCommittedOrdinal(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT ordinal, query_id, status, selected_order_json, plan_json, estimates_json, checksum
FROM query_state
WHERE ordinal <= ?
ORDER BY ordinal ASC`, cursor)
	if err != nil {
		return nil, newError(CodeStoreFailed, "load committed queries: %v", err)
	}
	defer rows.Close()
	var out []CommittedQuery
	for rows.Next() {
		var rec CommittedQuery
		var orderJSON, planJSON, estJSON string
		if err := rows.Scan(&rec.Ordinal, &rec.QueryID, &rec.Status, &orderJSON, &planJSON, &estJSON, &rec.Checksum); err != nil {
			return nil, newError(CodeStoreFailed, "scan committed query: %v", err)
		}
		if expected := checksumBytes([]byte(planJSON), []byte(estJSON)); expected != rec.Checksum {
			return nil, newError(CodeStoreCorrupt, "committed query checksum mismatch").
				with("query_id", rec.QueryID).with("ordinal", rec.Ordinal)
		}
		var order []string
		if err := json.Unmarshal([]byte(orderJSON), &order); err != nil {
			return nil, newError(CodeStoreCorrupt, "selected order JSON is corrupt").with("query_id", rec.QueryID)
		}
		var plan QueryPlan
		if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
			return nil, newError(CodeStoreCorrupt, "plan JSON is corrupt").with("query_id", rec.QueryID)
		}
		var est EstimateReport
		if err := json.Unmarshal([]byte(estJSON), &est); err != nil {
			return nil, newError(CodeStoreCorrupt, "estimate JSON is corrupt").with("query_id", rec.QueryID)
		}
		for id, pe := range est.Predicates {
			pe.Selectivity = Fixed(pe.SelectivityRaw)
			est.Predicates[id] = pe
		}
		rec.SelectedOrder = order
		rec.Plan = &plan
		rec.Estimates = &est
		out = append(out, rec)
	}
	return out, nil
}

func (s *Store) SaveCheckpoint(ctx context.Context, kind, engineeringVersion string, payload []byte) error {
	sum := checksumBytes(payload)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO checkpoints(kind, format_version, engineering_version, payload, payload_len, checksum)
VALUES(?,?,?,?,?,?)
ON CONFLICT(kind) DO UPDATE SET
 format_version=excluded.format_version,
 engineering_version=excluded.engineering_version,
 payload=excluded.payload,
 payload_len=excluded.payload_len,
 checksum=excluded.checksum`,
		kind, checkpointFormatVersion, engineeringVersion, payload, len(payload), sum)
	if err != nil {
		return newError(CodeStoreFailed, "save checkpoint: %v", err).with("kind", kind)
	}
	return nil
}

func (s *Store) LoadCheckpoint(ctx context.Context, kind, expectedEngineeringVersion string) ([]byte, error) {
	var formatVersion, payloadLen int
	var engineeringVersion, sum string
	var payload []byte
	err := s.db.QueryRowContext(ctx, `
SELECT format_version, engineering_version, payload, payload_len, checksum
FROM checkpoints WHERE kind=?`, kind).Scan(&formatVersion, &engineeringVersion, &payload, &payloadLen, &sum)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newError(CodeSnapshotTruncated, "checkpoint is missing").with("kind", kind)
	}
	if err != nil {
		return nil, newError(CodeStoreFailed, "load checkpoint: %v", err).with("kind", kind)
	}
	if formatVersion != checkpointFormatVersion {
		return nil, newError(CodeSnapshotMismatch, "checkpoint format version mismatch").
			with("expected", checkpointFormatVersion).with("actual", formatVersion)
	}
	if len(payload) != payloadLen {
		return nil, newError(CodeSnapshotTruncated, "checkpoint payload length does not match declared length").
			with("kind", kind).with("expected_len", payloadLen).with("actual_len", len(payload))
	}
	if checksumBytes(payload) != sum {
		return nil, newError(CodeSnapshotTruncated, "checkpoint checksum mismatch").
			with("kind", kind).with("expected_checksum", sum)
	}
	if engineeringVersion != expectedEngineeringVersion {
		return nil, newError(CodeSnapshotMismatch, "engineering snapshot version mismatch").
			with("expected", expectedEngineeringVersion).with("actual", engineeringVersion).with("kind", kind)
	}
	return payload, nil
}

func (s *Store) InjectTruncatedCheckpoint(ctx context.Context, kind string) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE checkpoints
SET payload = substr(payload, 1, CASE WHEN length(payload) > 1 THEN length(payload)/2 ELSE 0 END)
WHERE kind=?`, kind)
	if err != nil {
		return newError(CodeStoreFailed, "inject truncated checkpoint: %v", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return newError(CodeInputInvalid, "cannot truncate missing checkpoint").with("kind", kind)
	}
	return nil
}

func SQLitePathHint(path string) string {
	return fmt.Sprintf("sqlite://%s", path)
}
