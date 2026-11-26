package mirror

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	_ "modernc.org/sqlite"
)

// CheckpointState is the complete committed state needed to continue after a
// process restart. It deliberately contains no uncommitted task data.
type CheckpointState struct {
	SessionID        string             `json:"sessionId"`
	SourceVersion    string             `json:"sourceVersion"`
	AlgorithmVersion string             `json:"algorithmVersion"`
	WindowSeq        int                `json:"windowSeq"`
	LastWindowID     string             `json:"lastWindowId"`
	EventsConsumed   int                `json:"eventsConsumed"`
	Endpoints        []Endpoint         `json:"endpoints"`
	Generations      []ScreenGeneration `json:"generations,omitempty"`
	Screens          []ScreenCommit     `json:"screens"`
	WindowHistory    []WindowCommit     `json:"windowHistory"`
	ContentHash      string             `json:"contentHash"`
}

func ComputeCheckpointHash(s CheckpointState) string {
	cp := s
	cp.ContentHash = ""
	return HashJSON(cp)
}

type RejectedCheckpoint struct {
	WindowSeq int    `json:"windowSeq"`
	WindowID  string `json:"windowId"`
	Reason    string `json:"reason"`
}

type CheckpointStore struct {
	mu sync.Mutex
	db *sql.DB
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	if path == "" {
		return nil, errf(CodeState, "checkpoint.path", "checkpoint path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, errf(CodeState, "checkpoint.open", "open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	store := &CheckpointStore{db: db}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, errf(CodeState, "checkpoint.pragma", "set busy_timeout: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, errf(CodeState, "checkpoint.pragma", "enable foreign keys: %v", err)
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoints (
			session_id TEXT NOT NULL,
			window_seq INTEGER NOT NULL,
			window_id TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			source_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			events_consumed INTEGER NOT NULL,
			state_json TEXT NOT NULL,
			PRIMARY KEY (session_id, window_seq)
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_components (
			session_id TEXT NOT NULL,
			window_seq INTEGER NOT NULL,
			component TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			PRIMARY KEY (session_id, window_seq, component),
			FOREIGN KEY (session_id, window_seq) REFERENCES checkpoints(session_id, window_seq) ON DELETE CASCADE
		)`,
	}
	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, errf(CodeState, "checkpoint.schema", "create schema: %v", err)
		}
	}
	return store, nil
}

func (s *CheckpointStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SaveWindow atomically writes one whole committed window. Calls are
// serialized by a mutex and the sequence must advance by exactly one.
func (s *CheckpointStore) SaveWindow(ctx context.Context, state CheckpointState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state.SessionID == "" {
		return errf(CodeState, "checkpoint.session_id", "required value is missing")
	}
	if state.SourceVersion == "" || state.AlgorithmVersion == "" {
		return errf(CodeState, "checkpoint.version", "source and algorithm versions are required")
	}
	state.NormalizeSlices()
	computed := ComputeCheckpointHash(state)
	if state.ContentHash != "" && state.ContentHash != computed {
		return &Error{Code: CodeState, Path: "checkpoint.content_hash",
			Message: "provided content hash does not match checkpoint state"}
	}
	state.ContentHash = computed
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return errf(CodeState, "checkpoint.state", "marshal state: %v", err)
	}
	var latestSeq sql.NullInt64
	var latestHash sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT window_seq, content_hash FROM checkpoints WHERE session_id = ? ORDER BY window_seq DESC LIMIT 1`,
		state.SessionID).Scan(&latestSeq, &latestHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return errf(CodeState, "checkpoint.read", "read latest checkpoint: %v", err)
	}
	if latestSeq.Valid {
		if state.WindowSeq == int(latestSeq.Int64) {
			if latestHash.String == computed {
				return nil
			}
			return &Error{Code: CodeState, Path: "checkpoint.window_seq",
				Message: fmt.Sprintf("window %d was already committed with a different state", state.WindowSeq)}
		}
		if state.WindowSeq != int(latestSeq.Int64)+1 {
			return &Error{Code: CodeState, Path: "checkpoint.window_seq",
				Message: fmt.Sprintf("expected window sequence %d, got %d", latestSeq.Int64+1, state.WindowSeq)}
		}
	} else if state.WindowSeq != 0 {
		return &Error{Code: CodeState, Path: "checkpoint.window_seq",
			Message: "first checkpoint must be window sequence 0"}
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return errf(CodeState, "checkpoint.begin", "begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO checkpoints
		 (session_id, window_seq, window_id, content_hash, source_version, algorithm_version, events_consumed, state_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		state.SessionID, state.WindowSeq, state.LastWindowID, state.ContentHash,
		state.SourceVersion, state.AlgorithmVersion, state.EventsConsumed, string(stateJSON)); err != nil {
		return errf(CodeState, "checkpoint.insert", "insert checkpoint: %v", err)
	}
	components, err := checkpointComponents(state)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		payload := components[name]
		data, err := json.Marshal(payload)
		if err != nil {
			return errf(CodeState, "checkpoint.component", "marshal component %s: %v", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO checkpoint_components
			 (session_id, window_seq, component, payload_hash, payload_json)
			 VALUES (?, ?, ?, ?, ?)`,
			state.SessionID, state.WindowSeq, name, HashJSON(payload), string(data)); err != nil {
			return errf(CodeState, "checkpoint.component", "insert component %s: %v", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return errf(CodeState, "checkpoint.commit", "commit checkpoint: %v", err)
	}
	return nil
}

func (s *CheckpointState) NormalizeSlices() {
	sortEndpoints(s.Endpoints)
	sort.Slice(s.Generations, func(i, j int) bool { return s.Generations[i].ScreenID < s.Generations[j].ScreenID })
	sort.Slice(s.Screens, func(i, j int) bool { return s.Screens[i].ScreenID < s.Screens[j].ScreenID })
	for i := range s.Screens {
		sort.Slice(s.Screens[i].Packets, func(a, b int) bool {
			return s.Screens[i].Packets[a].Seq < s.Screens[i].Packets[b].Seq
		})
	}
	sort.Slice(s.WindowHistory, func(i, j int) bool {
		return s.WindowHistory[i].Sequence < s.WindowHistory[j].Sequence
	})
}

// RestoreLatest validates every component of the newest checkpoint. A damaged
// window is deleted atomically and the previous checkpoint is returned.
func (s *CheckpointStore) RestoreLatest(ctx context.Context, sessionID string) (*CheckpointState, []RejectedCheckpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx,
		`SELECT window_seq, window_id, content_hash, source_version, algorithm_version, events_consumed, state_json
		 FROM checkpoints WHERE session_id = ? ORDER BY window_seq DESC`, sessionID)
	if err != nil {
		return nil, nil, errf(CodeState, "checkpoint.restore", "query checkpoints: %v", err)
	}
	defer rows.Close()
	type rowData struct {
		seq, events                            int
		id, hash, source, algorithm, stateJSON string
	}
	var candidates []rowData
	for rows.Next() {
		var r rowData
		var seq int64
		var events int64
		if err := rows.Scan(&seq, &r.id, &r.hash, &r.source, &r.algorithm, &events, &r.stateJSON); err != nil {
			return nil, nil, errf(CodeState, "checkpoint.restore", "scan checkpoint: %v", err)
		}
		r.seq = int(seq)
		r.events = int(events)
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, errf(CodeState, "checkpoint.restore", "iterate checkpoints: %v", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, errf(CodeState, "checkpoint.restore", "close checkpoint rows: %v", err)
	}
	rejected := []RejectedCheckpoint{}
	for _, candidate := range candidates {
		state, reason := s.validateCheckpoint(ctx, candidate.seq, candidate.id, candidate.hash,
			candidate.source, candidate.algorithm, candidate.events, candidate.stateJSON)
		if reason == "" {
			return state, rejected, nil
		}
		rejected = append(rejected, RejectedCheckpoint{WindowSeq: candidate.seq, WindowID: candidate.id, Reason: reason})
		if err := s.deleteCheckpoint(ctx, sessionID, candidate.seq); err != nil {
			return nil, rejected, err
		}
	}
	return nil, rejected, nil
}

func (s *CheckpointStore) validateCheckpoint(ctx context.Context, seq int, id, contentHash, source, algorithm string, events int, stateJSON string) (*CheckpointState, string) {
	var state CheckpointState
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		return nil, "state JSON is invalid: " + err.Error()
	}
	if ComputeCheckpointHash(state) != contentHash || state.ContentHash != contentHash {
		return nil, "checkpoint content hash mismatch"
	}
	if state.WindowSeq != seq || state.LastWindowID != id {
		return nil, "checkpoint metadata does not match state"
	}
	if state.SourceVersion != source || state.AlgorithmVersion != algorithm || state.EventsConsumed != events {
		return nil, "checkpoint version or event cursor mismatch"
	}
	expected, err := checkpointComponents(state)
	if err != nil {
		return nil, err.Error()
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT component, payload_hash, payload_json FROM checkpoint_components
		 WHERE session_id = ? AND window_seq = ? ORDER BY component`, state.SessionID, seq)
	if err != nil {
		return nil, "query components: " + err.Error()
	}
	defer rows.Close()
	count := 0
	found := map[string]bool{}
	for rows.Next() {
		var name, hash, payload string
		if err := rows.Scan(&name, &hash, &payload); err != nil {
			return nil, "scan component: " + err.Error()
		}
		count++
		expectedPayload, ok := expected[name]
		if !ok {
			return nil, "unexpected component " + name
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(payload)))
		dec.UseNumber()
		var got any
		if err := dec.Decode(&got); err != nil {
			return nil, "component " + name + " JSON is invalid"
		}
		gotHash := HashJSON(got)
		if gotHash != hash || hash != HashJSON(expectedPayload) {
			return nil, "component " + name + " hash mismatch"
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, "iterate components: " + err.Error()
	}
	if count != len(expected) {
		return nil, "component count mismatch"
	}
	for name := range expected {
		if !found[name] {
			return nil, "missing component " + name
		}
	}
	return &state, ""
}

func checkpointComponents(state CheckpointState) (map[string]any, error) {
	components := map[string]any{
		"endpoints":   state.Endpoints,
		"events":      map[string]any{"consumed": state.EventsConsumed},
		"generations": state.Generations,
	}
	for _, screen := range state.Screens {
		components["screen:"+screen.ScreenID] = screen
	}
	if len(state.WindowHistory) > 0 {
		last := state.WindowHistory[len(state.WindowHistory)-1]
		components["window:"+last.WindowID] = last
	}
	return components, nil
}

func (s *CheckpointStore) deleteCheckpoint(ctx context.Context, sessionID string, seq int) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return errf(CodeState, "checkpoint.reject", "begin reject transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM checkpoint_components WHERE session_id = ? AND window_seq = ?`, sessionID, seq); err != nil {
		return errf(CodeState, "checkpoint.reject", "delete components: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM checkpoints WHERE session_id = ? AND window_seq = ?`, sessionID, seq); err != nil {
		return errf(CodeState, "checkpoint.reject", "delete checkpoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return errf(CodeState, "checkpoint.reject", "commit reject: %v", err)
	}
	return nil
}

func sortEndpoints(endpoints []Endpoint) {
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].ScreenID < endpoints[j].ScreenID })
}

func sortStrings(values []string) {
	sort.Strings(values)
}

// CheckpointSummary exposes the latest durable cursor to the diagnostic page.
type CheckpointSummary struct {
	Available      bool   `json:"available"`
	WindowSeq      int    `json:"windowSeq"`
	LastWindowID   string `json:"lastWindowId"`
	EventsConsumed int    `json:"eventsConsumed"`
	ContentHash    string `json:"contentHash"`
}

func SummaryOf(state *CheckpointState) CheckpointSummary {
	if state == nil {
		return CheckpointSummary{}
	}
	return CheckpointSummary{Available: true, WindowSeq: state.WindowSeq,
		LastWindowID: state.LastWindowID, EventsConsumed: state.EventsConsumed,
		ContentHash: state.ContentHash}
}
