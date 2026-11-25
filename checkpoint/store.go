package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"lipsync/deps"
	"lipsync/domain"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS windows (
  window_index INTEGER PRIMARY KEY,
  start_frame INTEGER NOT NULL,
  end_frame INTEGER NOT NULL,
  state TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  params_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  consumed_json TEXT NOT NULL,
  packet_states_json TEXT NOT NULL,
  commit_seq INTEGER NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS frame_results (
  window_index INTEGER NOT NULL,
  frame INTEGER NOT NULL,
  channel_id TEXT NOT NULL,
  accepted_json TEXT NOT NULL,
  delayed_json TEXT NOT NULL,
  preempted_json TEXT NOT NULL,
  resumed_json TEXT NOT NULL,
  occupied_slots INTEGER NOT NULL,
  used_budget INTEGER NOT NULL,
  PRIMARY KEY (window_index, frame, channel_id)
);
CREATE TABLE IF NOT EXISTS preempt_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  window_index INTEGER NOT NULL,
  frame INTEGER NOT NULL,
  channel_id TEXT NOT NULL,
  packet_id TEXT NOT NULL,
  by_packet_id TEXT NOT NULL,
  delivered INTEGER NOT NULL,
  remaining INTEGER NOT NULL,
  recovery_frame INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS resume_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  window_index INTEGER NOT NULL,
  frame INTEGER NOT NULL,
  channel_id TEXT NOT NULL,
  packet_id TEXT NOT NULL,
  protected_until INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS abandon_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  window_index INTEGER NOT NULL,
  frame INTEGER NOT NULL,
  channel_id TEXT NOT NULL,
  packet_id TEXT NOT NULL,
  reason TEXT NOT NULL,
  available_frames INTEGER NOT NULL,
  required_frames INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS frame_cache (
  node_id TEXT PRIMARY KEY,
  cache_key TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  frame INTEGER NOT NULL,
  result_json TEXT NOT NULL
);
`

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

type WindowCommit struct {
	Index            int
	StartFrame       int64
	EndFrame         int64
	State            string
	ContentHash      string
	ParamsVersion    string
	AlgorithmVersion string
	Frames           []domain.FrameResult
	Preemptions      []domain.PreemptEvent
	Resumes          []domain.ResumeEvent
	Abandonments     []domain.AbandonEvent
	PacketStates     []domain.PacketState
	Consumed         map[string]int64
}

type Checkpoint struct {
	Has              bool
	LastWindow       int
	NextFrame        int64
	Frames           []domain.FrameResult
	Preemptions      []domain.PreemptEvent
	Resumes          []domain.ResumeEvent
	Abandonments     []domain.AbandonEvent
	PacketStates     []domain.PacketState
	Consumed         map[string]int64
	CommitSequence   []int
	ContentHash      string
	ParamsVersion    string
	AlgorithmVersion string
	Windows          []StoredWindow
}

type StoredWindow struct {
	Index          int   `json:"index"`
	StartFrame     int64 `json:"start_frame"`
	EndFrame       int64 `json:"end_frame"`
	CommitSequence int   `json:"commit_sequence"`
	Preemptions    int   `json:"preemptions"`
}

func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		dsn = fmt.Sprintf("file:lipsync-%d?mode=memory&cache=shared&_txlock=immediate", time.Now().UnixNano())
	} else if len(path) > 0 && path[0] != ':' && !containsQuery(path) {
		dsn = path + "?_txlock=immediate&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES('schema','1') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func containsQuery(path string) bool {
	for _, r := range path {
		if r == '?' {
			return true
		}
	}
	return false
}

func (s *Store) Close() error { return s.db.Close() }

// SaveWindow commits every artifact for one frame window in one serialized
// SQLite transaction. A repeated index is idempotent and returns its original
// commit sequence.
func (s *Store) SaveWindow(commit WindowCommit) (int, error) {
	if commit.State != string(domain.WindowCommitted) {
		return 0, &domain.Error{Code: domain.CodeState, Path: "window.state", State: commit.State, Message: "only committed windows may be checkpointed"}
	}
	if commit.Index < 0 || commit.EndFrame < commit.StartFrame {
		return 0, domain.NewError(domain.CodeState, "window", "invalid window range")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var existing int
	err = conn.QueryRowContext(ctx, `SELECT commit_seq FROM windows WHERE window_index=?`, commit.Index).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	rollback := true
	defer func() {
		if rollback {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	var sequence int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(commit_seq),0)+1 FROM windows`).Scan(&sequence); err != nil {
		return 0, err
	}
	consumed, _ := json.Marshal(commit.Consumed)
	states, _ := json.Marshal(commit.PacketStates)
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO windows(window_index,start_frame,end_frame,state,content_hash,params_version,algorithm_version,consumed_json,packet_states_json,commit_seq)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		commit.Index, commit.StartFrame, commit.EndFrame, commit.State,
		commit.ContentHash, commit.ParamsVersion, commit.AlgorithmVersion,
		string(consumed), string(states), sequence); err != nil {
		return 0, err
	}
	for _, frame := range commit.Frames {
		for _, channel := range frame.Channels {
			accepted, _ := json.Marshal(channel.Accepted)
			delayed, _ := json.Marshal(channel.Delayed)
			preempted, _ := json.Marshal(channel.Preempted)
			resumed, _ := json.Marshal(channel.Resumed)
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO frame_results(window_index,frame,channel_id,accepted_json,delayed_json,preempted_json,resumed_json,occupied_slots,used_budget)
				VALUES(?,?,?,?,?,?,?,?,?)`,
				commit.Index, frame.Frame, channel.ChannelID, string(accepted), string(delayed),
				string(preempted), string(resumed), channel.OccupiedSlot, channel.UsedBudget); err != nil {
				return 0, err
			}
		}
	}
	for _, event := range commit.Preemptions {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO preempt_events(window_index,frame,channel_id,packet_id,by_packet_id,delivered,remaining,recovery_frame)
			VALUES(?,?,?,?,?,?,?,?)`,
			commit.Index, event.Frame, event.ChannelID, event.PacketID, event.ByPacketID,
			event.DeliveredFrames, event.RemainingFrames, event.RecoveryFrame); err != nil {
			return 0, err
		}
	}
	for _, event := range commit.Resumes {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO resume_events(window_index,frame,channel_id,packet_id,protected_until)
			VALUES(?,?,?,?,?)`,
			commit.Index, event.Frame, event.ChannelID, event.PacketID, event.ProtectedUntil); err != nil {
			return 0, err
		}
	}
	for _, event := range commit.Abandonments {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO abandon_events(window_index,frame,channel_id,packet_id,reason,available_frames,required_frames)
			VALUES(?,?,?,?,?,?,?)`,
			commit.Index, event.Frame, event.ChannelID, event.PacketID, event.Reason,
			event.AvailableFrames, event.RequiredFrames); err != nil {
			return 0, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	rollback = false
	return sequence, nil
}

func (s *Store) Load(contentHash, algorithmVersion string) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	var checkpoint Checkpoint
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM windows`).Scan(&count); err != nil {
		return checkpoint, err
	}
	if count == 0 {
		return checkpoint, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT window_index,start_frame,end_frame,state,content_hash,params_version,algorithm_version,consumed_json,packet_states_json,commit_seq
		FROM windows ORDER BY window_index`)
	if err != nil {
		return checkpoint, err
	}
	type windowRow struct {
		index, seq               int
		start, end               int64
		state                    string
		hash, params, algorithm  string
		consumedJSON, statesJSON string
	}
	var windowRows []windowRow
	for rows.Next() {
		var row windowRow
		if err := rows.Scan(&row.index, &row.start, &row.end, &row.state, &row.hash, &row.params, &row.algorithm, &row.consumedJSON, &row.statesJSON, &row.seq); err != nil {
			_ = rows.Close()
			return checkpoint, err
		}
		windowRows = append(windowRows, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return checkpoint, err
	}
	_ = rows.Close()
	var lastState string
	var lastSequence int
	for _, row := range windowRows {
		if row.hash != contentHash {
			return checkpoint, &domain.Error{Code: domain.CodeState, Path: "checkpoint.content_hash", Message: "checkpoint belongs to different project content"}
		}
		if row.algorithm != algorithmVersion {
			return checkpoint, &domain.Error{Code: domain.CodeState, Path: "checkpoint.algorithm_version", Message: "checkpoint algorithm version mismatch"}
		}
		if row.seq <= lastSequence {
			return checkpoint, &domain.Error{Code: domain.CodeState, Path: "checkpoint.commit_seq", Message: "commit sequence is not strictly increasing"}
		}
		lastSequence = row.seq
		lastState = row.state
		checkpoint.Has = true
		checkpoint.LastWindow = row.index
		checkpoint.NextFrame = row.end + 1
		checkpoint.ContentHash = row.hash
		checkpoint.ParamsVersion = row.params
		checkpoint.AlgorithmVersion = row.algorithm
		checkpoint.CommitSequence = append(checkpoint.CommitSequence, row.seq)
		if err := json.Unmarshal([]byte(row.consumedJSON), &checkpoint.Consumed); err != nil {
			return checkpoint, err
		}
		if err := json.Unmarshal([]byte(row.statesJSON), &checkpoint.PacketStates); err != nil {
			return checkpoint, err
		}
		frames, err := s.loadWindowFrames(ctx, row.index)
		if err != nil {
			return checkpoint, err
		}
		checkpoint.Frames = append(checkpoint.Frames, frames...)
		preemptions, err := s.loadWindowPreemptions(ctx, row.index)
		if err != nil {
			return checkpoint, err
		}
		checkpoint.Preemptions = append(checkpoint.Preemptions, preemptions...)
		checkpoint.Windows = append(checkpoint.Windows, StoredWindow{
			Index: row.index, StartFrame: row.start, EndFrame: row.end,
			CommitSequence: row.seq, Preemptions: len(preemptions),
		})
		resumes, err := s.loadWindowResumes(ctx, row.index)
		if err != nil {
			return checkpoint, err
		}
		checkpoint.Resumes = append(checkpoint.Resumes, resumes...)
		abandonments, err := s.loadWindowAbandonments(ctx, row.index)
		if err != nil {
			return checkpoint, err
		}
		checkpoint.Abandonments = append(checkpoint.Abandonments, abandonments...)
	}
	if lastState != string(domain.WindowCommitted) {
		return checkpoint, &domain.Error{Code: domain.CodeState, Path: "checkpoint.window_state", State: lastState, Message: "checkpoint is not in committed state"}
	}
	if checkpoint.Consumed == nil {
		checkpoint.Consumed = map[string]int64{}
	}
	return checkpoint, nil
}

func (s *Store) loadWindowFrames(ctx context.Context, windowIndex int) ([]domain.FrameResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT frame,channel_id,accepted_json,delayed_json,preempted_json,resumed_json,occupied_slots,used_budget
		FROM frame_results WHERE window_index=? ORDER BY frame,rowid`, windowIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byFrame := map[int64]*domain.FrameResult{}
	var order []int64
	for rows.Next() {
		var (
			frame                                 int64
			channel                               domain.FrameChannelResult
			accepted, delayed, preempted, resumed string
		)
		if err := rows.Scan(&frame, &channel.ChannelID, &accepted, &delayed, &preempted, &resumed, &channel.OccupiedSlot, &channel.UsedBudget); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(accepted), &channel.Accepted)
		_ = json.Unmarshal([]byte(delayed), &channel.Delayed)
		_ = json.Unmarshal([]byte(preempted), &channel.Preempted)
		_ = json.Unmarshal([]byte(resumed), &channel.Resumed)
		result, ok := byFrame[frame]
		if !ok {
			result = &domain.FrameResult{Frame: frame, Channels: []domain.FrameChannelResult{}}
			byFrame[frame] = result
			order = append(order, frame)
		}
		result.Channels = append(result.Channels, channel)
	}
	var frames []domain.FrameResult
	for _, frame := range order {
		frames = append(frames, *byFrame[frame])
	}
	return frames, rows.Err()
}

func (s *Store) loadWindowPreemptions(ctx context.Context, windowIndex int) ([]domain.PreemptEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT frame,channel_id,packet_id,by_packet_id,delivered,remaining,recovery_frame
		FROM preempt_events WHERE window_index=? ORDER BY id`, windowIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []domain.PreemptEvent
	for rows.Next() {
		var event domain.PreemptEvent
		if err := rows.Scan(&event.Frame, &event.ChannelID, &event.PacketID, &event.ByPacketID,
			&event.DeliveredFrames, &event.RemainingFrames, &event.RecoveryFrame); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) loadWindowResumes(ctx context.Context, windowIndex int) ([]domain.ResumeEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT frame,channel_id,packet_id,protected_until
		FROM resume_events WHERE window_index=? ORDER BY id`, windowIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []domain.ResumeEvent
	for rows.Next() {
		var event domain.ResumeEvent
		if err := rows.Scan(&event.Frame, &event.ChannelID, &event.PacketID, &event.ProtectedUntil); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) loadWindowAbandonments(ctx context.Context, windowIndex int) ([]domain.AbandonEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT frame,channel_id,packet_id,reason,available_frames,required_frames
		FROM abandon_events WHERE window_index=? ORDER BY id`, windowIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []domain.AbandonEvent
	for rows.Next() {
		var event domain.AbandonEvent
		if err := rows.Scan(&event.Frame, &event.ChannelID, &event.PacketID, &event.Reason,
			&event.AvailableFrames, &event.RequiredFrames); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// Frame cache methods implement deps.Store, making invalidation reuse durable
// in the same SQLite database as arbitration checkpoints.
func (s *Store) GetFrame(nodeID string) (deps.DomainFrameEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entry deps.DomainFrameEntry
	var resultJSON string
	err := s.db.QueryRow(`SELECT node_id,cache_key,channel_id,frame,result_json FROM frame_cache WHERE node_id=?`, nodeID).
		Scan(&entry.NodeID, &entry.CacheKey, &entry.ChannelID, &entry.Frame, &resultJSON)
	if err != nil {
		return deps.DomainFrameEntry{}, false
	}
	if err := json.Unmarshal([]byte(resultJSON), &entry.Result); err != nil {
		return deps.DomainFrameEntry{}, false
	}
	return entry, true
}

func (s *Store) PutFrame(entry deps.DomainFrameEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	resultJSON, _ := json.Marshal(entry.Result)
	_, err := s.db.Exec(`
		INSERT INTO frame_cache(node_id,cache_key,channel_id,frame,result_json)
		VALUES(?,?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET cache_key=excluded.cache_key,channel_id=excluded.channel_id,frame=excluded.frame,result_json=excluded.result_json`,
		entry.NodeID, entry.CacheKey, entry.ChannelID, entry.Frame, string(resultJSON))
	return err
}

func (s *Store) DeleteFrame(nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM frame_cache WHERE node_id=?`, nodeID)
	return err
}
