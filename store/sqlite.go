package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "modernc.org/sqlite"

	"example.com/multiscreen-cast/kernel"
	"example.com/multiscreen-cast/protocol"
)

type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

type LoadStatus struct {
	Found       bool   `json:"found"`
	Invalidated bool   `json:"invalidated"`
	Reason      string `json:"reason,omitempty"`
	Rows        int    `json:"rows"`
}

type RowInfo struct {
	WindowIndex      int    `json:"window_index"`
	AlgorithmVersion string `json:"algorithm_version"`
	TopologyVersion  int    `json:"topology_version"`
	ContentHash      string `json:"content_hash"`
	SnapshotHash     string `json:"snapshot_hash"`
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, protocol.Errorf(protocol.CodeStore, "checkpoint", "$.path", "database path is required")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, protocol.Errorf(protocol.CodeStore, path, "$", "cannot create database directory: %v", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, protocol.Errorf(protocol.CodeStore, path, "$", "cannot open sqlite database: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	if err := s.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`CREATE TABLE IF NOT EXISTS cast_checkpoint_sessions (
			session_id TEXT PRIMARY KEY,
			algorithm_version TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			total_windows INTEGER NOT NULL,
			last_window INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS cast_checkpoints (
			session_id TEXT NOT NULL,
			window_index INTEGER NOT NULL,
			algorithm_version TEXT NOT NULL,
			topology_version INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			snapshot_hash TEXT NOT NULL,
			snapshot_json BLOB NOT NULL,
			PRIMARY KEY (session_id, window_index),
			FOREIGN KEY (session_id) REFERENCES cast_checkpoint_sessions(session_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cast_checkpoints_session_window
			ON cast_checkpoints(session_id, window_index)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return protocol.Errorf(protocol.CodeStore, s.path, "$", "sqlite initialization failed: %v", err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) SaveCheckpoint(snapshot kernel.RuntimeSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "checkpoint store is closed")
	}
	if snapshot.SessionID == "" {
		return protocol.Errorf(protocol.CodeStore, s.path, "$.session_id", "checkpoint session id is required")
	}
	if snapshot.CommittedWindows <= 0 {
		return protocol.Errorf(protocol.CodeStore, s.path, "$.committed_windows", "checkpoint window must be positive")
	}
	if snapshot.NextAction <= 0 {
		return protocol.Errorf(protocol.CodeStore, s.path, "$.next_action", "checkpoint next action must be positive")
	}
	payload, snapshotHash, err := kernel.SnapshotJSON(snapshot)
	if err != nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "%v", err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot begin checkpoint transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var lastWindow int
	var existingHash, contentHash, algorithm string
	err = tx.QueryRow(`SELECT last_window, content_hash, algorithm_version
		FROM cast_checkpoint_sessions WHERE session_id = ?`, snapshot.SessionID).
		Scan(&lastWindow, &contentHash, &algorithm)
	if err != nil && err != sql.ErrNoRows {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot read checkpoint session: %v", err)
	}
	if err == nil {
		if contentHash != snapshot.ContentHash {
			return protocol.Errorf(protocol.CodeStore, s.path, "$.content_hash",
				"checkpoint content hash changed from %s to %s", contentHash, snapshot.ContentHash)
		}
		if algorithm != snapshot.AlgorithmVersion {
			return protocol.Errorf(protocol.CodeStore, s.path, "$.algorithm_version",
				"checkpoint algorithm changed from %s to %s", algorithm, snapshot.AlgorithmVersion)
		}
	} else {
		lastWindow = 0
	}
	if snapshot.CommittedWindows < lastWindow {
		return protocol.Errorf(protocol.CodeStore, s.path, "$.committed_windows",
			"window commit order reversed: got %d after %d", snapshot.CommittedWindows, lastWindow)
	}
	if snapshot.CommittedWindows == lastWindow {
		err = tx.QueryRow(`SELECT snapshot_hash FROM cast_checkpoints
			WHERE session_id = ? AND window_index = ?`, snapshot.SessionID, snapshot.CommittedWindows).
			Scan(&existingHash)
		if err == nil {
			if existingHash == snapshotHash {
				return nil
			}
			return protocol.Errorf(protocol.CodeStore, s.path, "$.window",
				"window %d already exists with different content", snapshot.CommittedWindows)
		}
		if err != sql.ErrNoRows {
			return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot inspect existing checkpoint: %v", err)
		}
	}
	if snapshot.CommittedWindows != lastWindow+1 {
		return protocol.Errorf(protocol.CodeStore, s.path, "$.committed_windows",
			"checkpoint window gap: got %d after %d", snapshot.CommittedWindows, lastWindow)
	}
	if _, err := tx.Exec(`INSERT INTO cast_checkpoint_sessions
		(session_id, algorithm_version, content_hash, total_windows, last_window)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			algorithm_version = excluded.algorithm_version,
			content_hash = excluded.content_hash,
			total_windows = excluded.total_windows,
			last_window = excluded.last_window`,
		snapshot.SessionID, snapshot.AlgorithmVersion, snapshot.ContentHash,
		snapshot.TotalWindows, snapshot.CommittedWindows); err != nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot upsert checkpoint session: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO cast_checkpoints
		(session_id, window_index, algorithm_version, topology_version, content_hash, snapshot_hash, snapshot_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		snapshot.SessionID, snapshot.CommittedWindows, snapshot.AlgorithmVersion,
		snapshot.TopologyVersion, snapshot.ContentHash, snapshotHash, payload); err != nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot insert checkpoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot commit checkpoint: %v", err)
	}
	return nil
}

func (s *Store) LoadLatest(session *protocol.Session, algorithmVersion string) (*kernel.RuntimeSnapshot, LoadStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, LoadStatus{}, protocol.Errorf(protocol.CodeStore, s.path, "$", "checkpoint store is closed")
	}
	contentHash, hashErr := protocol.SessionContentHash(session)
	if hashErr != nil {
		return nil, LoadStatus{}, hashErr
	}
	rows, err := s.db.Query(`SELECT window_index, algorithm_version, topology_version,
		content_hash, snapshot_hash, snapshot_json
		FROM cast_checkpoints WHERE session_id = ? ORDER BY window_index ASC`, session.ID)
	if err != nil {
		return nil, LoadStatus{}, protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot query checkpoints: %v", err)
	}
	defer rows.Close()
	type loadedRow struct {
		window       int
		algorithm    string
		topology     int
		content      string
		snapshotHash string
		payload      []byte
	}
	var loaded []loadedRow
	for rows.Next() {
		var row loadedRow
		if err := rows.Scan(&row.window, &row.algorithm, &row.topology, &row.content, &row.snapshotHash, &row.payload); err != nil {
			return nil, LoadStatus{}, protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot scan checkpoint: %v", err)
		}
		loaded = append(loaded, row)
	}
	if err := rows.Err(); err != nil {
		return nil, LoadStatus{}, protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot iterate checkpoints: %v", err)
	}
	if err := rows.Close(); err != nil {
		return nil, LoadStatus{}, protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot close checkpoint rows: %v", err)
	}
	if len(loaded) == 0 {
		return nil, LoadStatus{Found: false}, nil
	}
	schedule := protocol.BuildSchedule(session)
	invalidReason := ""
	var latest *kernel.RuntimeSnapshot
	for i, row := range loaded {
		if row.window != i+1 {
			invalidReason = fmt.Sprintf("checkpoint window sequence is not contiguous at %d", row.window)
			break
		}
		if row.algorithm != algorithmVersion {
			invalidReason = fmt.Sprintf("algorithm version mismatch at window %d: stored %q expected %q",
				row.window, row.algorithm, algorithmVersion)
			break
		}
		if row.content != contentHash {
			invalidReason = fmt.Sprintf("content hash mismatch at window %d", row.window)
			break
		}
		if protocol.HashBytes(row.payload) != row.snapshotHash {
			invalidReason = fmt.Sprintf("snapshot hash mismatch at window %d", row.window)
			break
		}
		var snapshot kernel.RuntimeSnapshot
		if err := json.Unmarshal(row.payload, &snapshot); err != nil {
			invalidReason = fmt.Sprintf("snapshot JSON is corrupt at window %d: %v", row.window, err)
			break
		}
		if snapshot.SessionID != session.ID ||
			snapshot.AlgorithmVersion != algorithmVersion ||
			snapshot.ContentHash != contentHash {
			invalidReason = fmt.Sprintf("snapshot identity/version mismatch at window %d", row.window)
			break
		}
		if snapshot.CommittedWindows != row.window || snapshot.TopologyVersion != row.topology {
			invalidReason = fmt.Sprintf("snapshot metadata mismatch at window %d", row.window)
			break
		}
		if snapshot.TotalWindows != session.TotalWindows || len(snapshot.Trace.Windows) != row.window {
			invalidReason = fmt.Sprintf("snapshot window count mismatch at window %d", row.window)
			break
		}
		if snapshot.NextAction <= 0 || snapshot.NextAction > len(schedule) {
			invalidReason = fmt.Sprintf("snapshot next action is invalid at window %d", row.window)
			break
		}
		lastAction := schedule[snapshot.NextAction-1]
		if !lastAction.Frame || lastAction.WindowIndex+1 != row.window {
			invalidReason = fmt.Sprintf("snapshot does not end at frame window %d", row.window)
			break
		}
		expectedTopology := protocol.ExpectedTopologyVersionAt(schedule, snapshot.NextAction)
		if row.topology != expectedTopology || snapshot.TopologyVersion != expectedTopology {
			invalidReason = fmt.Sprintf("topology version mismatch at window %d: stored %d expected %d",
				row.window, row.topology, expectedTopology)
			break
		}
		if !validEndpoints(snapshot.Endpoints) || !validScreenIDs(snapshot.Screens, snapshot.Endpoints) {
			invalidReason = fmt.Sprintf("snapshot endpoint set is invalid at window %d", row.window)
			break
		}
		if !validEndpoints(snapshot.Trace.FinalEndpoints) ||
			!sameStringSet(snapshot.Trace.FinalEndpoints, snapshot.Endpoints) ||
			snapshot.Trace.FinalState != snapshot.State {
			invalidReason = fmt.Sprintf("snapshot terminal state/endpoints mismatch at window %d", row.window)
			break
		}
		if len(snapshot.Trace.Windows) == 0 ||
			!sameStringSet(snapshot.Trace.Windows[len(snapshot.Trace.Windows)-1].Endpoints, snapshot.Endpoints) {
			invalidReason = fmt.Sprintf("snapshot window endpoint set mismatch at window %d", row.window)
			break
		}
		copySnapshot := snapshot
		latest = &copySnapshot
	}
	if invalidReason != "" {
		if err := s.invalidateLocked(session.ID); err != nil {
			return nil, LoadStatus{}, err
		}
		return nil, LoadStatus{Found: true, Invalidated: true, Reason: invalidReason, Rows: len(loaded)}, nil
	}
	var metaLast int
	var metaContent, metaAlgorithm string
	if err := s.db.QueryRow(`SELECT last_window, content_hash, algorithm_version
		FROM cast_checkpoint_sessions WHERE session_id = ?`, session.ID).
		Scan(&metaLast, &metaContent, &metaAlgorithm); err != nil {
		if err != sql.ErrNoRows {
			return nil, LoadStatus{}, protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot query checkpoint metadata: %v", err)
		}
		if invalidateErr := s.invalidateLocked(session.ID); invalidateErr != nil {
			return nil, LoadStatus{}, invalidateErr
		}
		return nil, LoadStatus{Found: true, Invalidated: true, Reason: "checkpoint session metadata is missing", Rows: len(loaded)}, nil
	}
	if metaLast != len(loaded) || metaContent != contentHash || metaAlgorithm != algorithmVersion {
		if err := s.invalidateLocked(session.ID); err != nil {
			return nil, LoadStatus{}, err
		}
		return nil, LoadStatus{Found: true, Invalidated: true, Reason: "checkpoint session metadata mismatch", Rows: len(loaded)}, nil
	}
	return latest, LoadStatus{Found: true, Rows: len(loaded)}, nil
}

func (s *Store) invalidateLocked(sessionID string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot begin invalidation: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM cast_checkpoints WHERE session_id = ?`, sessionID); err != nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot delete invalid checkpoints: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM cast_checkpoint_sessions WHERE session_id = ?`, sessionID); err != nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot delete invalid checkpoint metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot commit invalidation: %v", err)
	}
	return nil
}

func (s *Store) Invalidate(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "checkpoint store is closed")
	}
	return s.invalidateLocked(sessionID)
}

func (s *Store) Rows(sessionID string) ([]RowInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, protocol.Errorf(protocol.CodeStore, s.path, "$", "checkpoint store is closed")
	}
	rows, err := s.db.Query(`SELECT window_index, algorithm_version, topology_version, content_hash, snapshot_hash
		FROM cast_checkpoints WHERE session_id = ? ORDER BY window_index ASC`, sessionID)
	if err != nil {
		return nil, protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot query checkpoint rows: %v", err)
	}
	defer rows.Close()
	var output []RowInfo
	for rows.Next() {
		var row RowInfo
		if err := rows.Scan(&row.WindowIndex, &row.AlgorithmVersion, &row.TopologyVersion, &row.ContentHash, &row.SnapshotHash); err != nil {
			return nil, protocol.Errorf(protocol.CodeStore, s.path, "$", "cannot scan checkpoint row: %v", err)
		}
		output = append(output, row)
	}
	return output, rows.Err()
}

func (s *Store) CorruptSnapshotForTest(sessionID string, window int, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "checkpoint store is closed")
	}
	result, err := s.db.Exec(`UPDATE cast_checkpoints SET snapshot_json = ?, snapshot_hash = ?
		WHERE session_id = ? AND window_index = ?`, payload, protocol.HashBytes(payload), sessionID, window)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return fmt.Errorf("checkpoint row not found")
	}
	return nil
}

func (s *Store) TamperTopologyVersionForTest(sessionID string, window, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return protocol.Errorf(protocol.CodeStore, s.path, "$", "checkpoint store is closed")
	}
	result, err := s.db.Exec(`UPDATE cast_checkpoints SET topology_version = ?
		WHERE session_id = ? AND window_index = ?`, version, sessionID, window)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return fmt.Errorf("checkpoint row not found")
	}
	return nil
}

func validEndpoints(endpoints []string) bool {
	seen := make(map[string]bool, len(endpoints))
	for _, id := range endpoints {
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	if !sort.StringsAreSorted(endpoints) {
		return false
	}
	return true
}

func validScreenIDs(screens []kernel.ScreenState, endpoints []string) bool {
	active := make(map[string]bool, len(endpoints))
	for _, id := range endpoints {
		active[id] = true
	}
	seen := make(map[string]bool, len(screens))
	for _, screen := range screens {
		if !active[screen.DisplayID] || seen[screen.DisplayID] {
			return false
		}
		seen[screen.DisplayID] = true
	}
	return true
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
