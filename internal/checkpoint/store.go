package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"atelier/internal/hashutil"
	"atelier/internal/model"

	_ "modernc.org/sqlite"
)

const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
	StatusFailed    = "failed"
	StatusInvalid   = "invalid"

	SourceForcedTermination = "forced_termination"
	SourceActiveCancel      = "active_cancel"
	SourceCompleted         = "completed_checkpoint"
	SourceNone              = "none"
)

type Store struct {
	mu sync.Mutex
	db *sql.DB
}

type VerifyRequest struct {
	ExpectedScenarioHash  string
	ExpectedAlgorithm     string
	ExpectedParamsVersion map[string]string
	ExpectedZoneOrder     []string
}

type RecoverySnapshot struct {
	SessionID        string
	Status           string
	RecoverySource   string
	ScenarioHash     string
	AlgorithmVersion string
	Order            []string
	Zones            []model.ZoneState
	Valid            bool
	Discarded        bool
	DiscardReason    string
}

type sessionInfo struct {
	ID               string
	ScenarioHash     string
	AlgorithmVersion string
	Status           string
	ZoneOrder        []string
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			scenario_hash TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			status TEXT NOT NULL,
			zone_order TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS zone_checkpoints (
			session_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			zone_id TEXT NOT NULL,
			params_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			payload BLOB NOT NULL,
			committed_at TEXT NOT NULL,
			PRIMARY KEY(session_id, zone_id),
			UNIQUE(session_id, ordinal)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_created ON sessions(created_at DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("sqlite migration: %w", err)
		}
	}
	return nil
}

func (s *Store) BeginSession(ctx context.Context, scenarioHash string, zoneOrder []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	orderJSON, _ := json.Marshal(zoneOrder)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := fmt.Sprintf("session-%d-%s", time.Now().UnixNano(), hashutil.HashBytes(orderJSON)[:8])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions
		(id, scenario_hash, algorithm_version, status, zone_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, scenarioHash, model.AlgorithmVersion, StatusRunning, string(orderJSON), now, now)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) SaveZone(ctx context.Context, sessionID string, state model.ZoneState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	contentHash := hashutil.HashBytes(payload)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var orderJSON, status string
	if err := tx.QueryRowContext(ctx, `SELECT zone_order, status FROM sessions WHERE id = ?`, sessionID).
		Scan(&orderJSON, &status); err != nil {
		return err
	}
	if status != StatusRunning {
		return fmt.Errorf("session %s is not running (status=%s)", sessionID, status)
	}
	var order []string
	if err := json.Unmarshal([]byte(orderJSON), &order); err != nil {
		return err
	}
	position := -1
	for i, zoneID := range order {
		if zoneID == state.ZoneID {
			position = i
			break
		}
	}
	if position < 0 {
		return fmt.Errorf("zone %s is not in the stable session order", state.ZoneID)
	}
	var maxOrdinal int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ordinal), -1) FROM zone_checkpoints WHERE session_id = ?`,
		sessionID).Scan(&maxOrdinal); err != nil {
		return err
	}
	if position != maxOrdinal+1 {
		return fmt.Errorf("zone %s commit ordinal %d violates stable order; expected %d",
			state.ZoneID, position, maxOrdinal+1)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO zone_checkpoints
		(session_id, ordinal, zone_id, params_version, algorithm_version, content_hash, payload, committed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, zone_id) DO UPDATE SET
			ordinal=excluded.ordinal,
			params_version=excluded.params_version,
			algorithm_version=excluded.algorithm_version,
			content_hash=excluded.content_hash,
			payload=excluded.payload,
			committed_at=excluded.committed_at`,
		sessionID, position, state.ZoneID, state.ParamsVersion,
		model.AlgorithmVersion, contentHash, payload, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishSession(ctx context.Context, sessionID, status string) error {
	if status == "" {
		status = StatusFailed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`, status, now, sessionID)
	return err
}

func (s *Store) LoadLatest(ctx context.Context, req VerifyRequest) (RecoverySnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var info sessionInfo
	var orderJSON string
	err := s.db.QueryRowContext(ctx, `SELECT id, scenario_hash, algorithm_version, status, zone_order, created_at
		FROM sessions WHERE scenario_hash = ? ORDER BY created_at DESC LIMIT 1`, req.ExpectedScenarioHash).
		Scan(&info.ID, &info.ScenarioHash, &info.AlgorithmVersion, &info.Status, &orderJSON, new(string))
	if err == sql.ErrNoRows {
		return RecoverySnapshot{RecoverySource: SourceNone, Valid: false}, nil
	}
	if err != nil {
		return RecoverySnapshot{}, err
	}
	if err := json.Unmarshal([]byte(orderJSON), &info.ZoneOrder); err != nil {
		return RecoverySnapshot{}, err
	}
	snap := RecoverySnapshot{
		SessionID:        info.ID,
		Status:           info.Status,
		ScenarioHash:     info.ScenarioHash,
		AlgorithmVersion: info.AlgorithmVersion,
		Order:            info.ZoneOrder,
		RecoverySource:   recoverySource(info.Status),
	}
	if info.Status == StatusInvalid {
		snap.Valid = false
		snap.Discarded = true
		snap.DiscardReason = "session was previously invalidated"
		return snap, nil
	}
	discard := func(reason string) (RecoverySnapshot, error) {
		if err := s.invalidateSessionLocked(ctx, info.ID, reason); err != nil {
			return RecoverySnapshot{}, err
		}
		snap.Valid = false
		snap.Discarded = true
		snap.DiscardReason = reason
		snap.Zones = nil
		return snap, nil
	}
	if info.ScenarioHash != req.ExpectedScenarioHash {
		return discard("scenario content hash mismatch")
	}
	if info.AlgorithmVersion != req.ExpectedAlgorithm {
		return discard("session algorithm version mismatch")
	}
	if !sameOrder(info.ZoneOrder, req.ExpectedZoneOrder) {
		return discard("zone commit order mismatch")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ordinal, zone_id, params_version, algorithm_version, content_hash, payload
		FROM zone_checkpoints WHERE session_id = ? ORDER BY ordinal`, info.ID)
	if err != nil {
		return RecoverySnapshot{}, err
	}
	type rowData struct {
		ordinal          int
		zoneID           string
		paramsVersion    string
		algorithmVersion string
		contentHash      string
		payload          []byte
	}
	var rowDataList []rowData
	for rows.Next() {
		var row rowData
		if err := rows.Scan(&row.ordinal, &row.zoneID, &row.paramsVersion, &row.algorithmVersion, &row.contentHash, &row.payload); err != nil {
			rows.Close()
			return RecoverySnapshot{}, err
		}
		rowDataList = append(rowDataList, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return RecoverySnapshot{}, err
	}
	rows.Close()
	var zones []model.ZoneState
	nextOrdinal := 0
	for _, row := range rowDataList {
		if row.ordinal != nextOrdinal {
			return discard(fmt.Sprintf("non-contiguous checkpoint ordinal %d", row.ordinal))
		}
		if row.algorithmVersion != req.ExpectedAlgorithm {
			return discard("zone checkpoint algorithm version mismatch")
		}
		expectedVersion, ok := req.ExpectedParamsVersion[row.zoneID]
		if !ok || expectedVersion != row.paramsVersion {
			return discard(fmt.Sprintf("zone %s parameter version mismatch", row.zoneID))
		}
		if hashutil.HashBytes(row.payload) != row.contentHash {
			return discard(fmt.Sprintf("zone %s content hash mismatch", row.zoneID))
		}
		var state model.ZoneState
		if err := json.Unmarshal(row.payload, &state); err != nil {
			return discard(fmt.Sprintf("zone %s payload is invalid", row.zoneID))
		}
		if state.ZoneID != row.zoneID {
			return discard(fmt.Sprintf("zone %s checkpoint identity mismatch", row.zoneID))
		}
		zones = append(zones, state)
		nextOrdinal++
	}
	snap.Valid = true
	snap.Zones = zones
	return snap, nil
}

func recoverySource(status string) string {
	switch status {
	case StatusRunning:
		return SourceForcedTermination
	case StatusCancelled:
		return SourceActiveCancel
	case StatusCompleted:
		return SourceCompleted
	case StatusFailed:
		return "failed"
	default:
		return SourceNone
	}
}

func (s *Store) invalidateSessionLocked(ctx context.Context, sessionID, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM zone_checkpoints WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`,
		StatusInvalid, time.Now().UTC().Format(time.RFC3339Nano), sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Store) SessionCount(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n, err
}

func (s *Store) CheckpointRows(ctx context.Context, sessionID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT zone_id FROM zone_checkpoints WHERE session_id = ? ORDER BY ordinal`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func IsSQLiteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 16)
	_, _ = f.Read(buf)
	return strings.HasPrefix(string(buf), "SQLite format 3")
}
