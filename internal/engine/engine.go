package engine

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"roomreverb/internal/config"
	"roomreverb/internal/model"
	"roomreverb/internal/sched"
	"roomreverb/internal/store"
)

type RoomState string

const (
	StatePending   RoomState = "pending"
	StateAnalyzing RoomState = "connectivity_analyzing"
	StateSolving   RoomState = "parameter_solving"
	StateCommitted RoomState = "committed"
	StateFailed    RoomState = "failed"
)

type CacheStats struct {
	RoomHits       int `json:"room_hits"`
	RoomMisses     int `json:"room_misses"`
	ListenerHits   int `json:"listener_hits"`
	ListenerMisses int `json:"listener_misses"`
	PathReused     int `json:"path_reused"`
	PathRecomputed int `json:"path_recomputed"`
}

type InvalidationItem struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	RoomID  string   `json:"room_id,omitempty"`
	Reasons []string `json:"reasons"`
	Reason  string   `json:"reason"`
	Reused  bool     `json:"reused"`
}

type InvalidationPlan struct {
	Items            []InvalidationItem `json:"items"`
	InvalidRooms     []string           `json:"invalid_rooms"`
	InvalidListeners []string           `json:"invalid_listeners"`
	ReusedRooms      []string           `json:"reused_rooms"`
	ReusedListeners  []string           `json:"reused_listeners"`
	GeneratedAt      time.Time          `json:"generated_at"`
}

type StateRoom struct {
	ID       string    `json:"id"`
	State    RoomState `json:"state"`
	Progress string    `json:"progress"`
	Revision int       `json:"revision"`
	Error    string    `json:"error,omitempty"`
}

type Snapshot struct {
	Config           config.Config      `json:"config"`
	Level            *model.Level       `json:"level"`
	States           []StateRoom        `json:"states"`
	Committed        int                `json:"committed"`
	Total            int                `json:"total"`
	Progress         string             `json:"progress"`
	Result           *model.FullResult  `json:"result"`
	LastPlan         *InvalidationPlan  `json:"last_plan"`
	CacheStats       CacheStats         `json:"cache_stats"`
	QueueEvents      []sched.QueueEvent `json:"queue_events"`
	RecoveredRooms   int                `json:"recovered_rooms"`
	SnapshotInvalid  bool               `json:"snapshot_invalid"`
	LastError        *model.Error       `json:"last_error,omitempty"`
	CommitWriteOrder []string           `json:"commit_write_order"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

type Engine struct {
	mu               sync.Mutex
	cfg              config.Config
	level            *model.Level
	store            *store.Store
	pool             *sched.Pool
	states           map[string]RoomState
	stateErrors      map[string]string
	revisions        map[string]int
	result           *model.FullResult
	roomCache        map[string]model.RoomResult
	pathCache        map[string]model.PathResult
	plans            []InvalidationPlan
	lastPlan         *InvalidationPlan
	stats            CacheStats
	recoveredRooms   int
	snapshotInvalid  bool
	lastError        *model.Error
	commitWriteOrder []string
}

var legalTransitions = map[RoomState]map[RoomState]bool{
	StatePending: {
		StateAnalyzing: true,
	},
	StateAnalyzing: {
		StateSolving: true,
		StateFailed:  true,
	},
	StateSolving: {
		StateCommitted: true,
		StateFailed:    true,
	},
	StateFailed: {
		StateAnalyzing: true,
	},
	StateCommitted: {
		// Committed is terminal for ordinary transitions. A new parameter or
		// geometry revision must pass through OpenDraft.
	},
}

func New(cfg config.Config, level *model.Level, st *store.Store) *Engine {
	pool := sched.NewPool(cfg.MaxParallelism, cfg.SampleBufferLimit)
	states := map[string]RoomState{}
	revisions := map[string]int{}
	for _, room := range level.Rooms {
		states[room.ID] = StatePending
		revisions[room.ID] = 1
	}
	return &Engine{
		cfg:         cfg,
		level:       level,
		store:       st,
		pool:        pool,
		states:      states,
		stateErrors: map[string]string{},
		revisions:   revisions,
		roomCache:   map[string]model.RoomResult{},
		pathCache:   map[string]model.PathResult{},
	}
}

func (e *Engine) Config() config.Config { return e.cfg }

func (e *Engine) Level() *model.Level {
	e.mu.Lock()
	defer e.mu.Unlock()
	return CloneLevel(e.level)
}

func (e *Engine) Transition(roomID string, to RoomState) *model.Error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.transitionLocked(roomID, to)
}

func (e *Engine) OpenDraft(roomID string) *model.Error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.openDraftLocked(roomID)
}

func (e *Engine) openDraftLocked(roomID string) *model.Error {
	state, ok := e.states[roomID]
	if !ok {
		return model.NewError(model.CodeState, roomID, "unknown room")
	}
	if state == StateAnalyzing || state == StateSolving {
		return model.NewError(model.CodeState, roomID, "cannot open draft while solve is running")
	}
	if state == StateCommitted {
		e.states[roomID] = StatePending
		e.revisions[roomID]++
	}
	return nil
}

func (e *Engine) transitionLocked(roomID string, to RoomState) *model.Error {
	from, ok := e.states[roomID]
	if !ok {
		return model.NewError(model.CodeState, roomID, "unknown room")
	}
	if from == to {
		return nil
	}
	if !legalTransitions[from][to] {
		err := model.NewError(model.CodeState, roomID, fmt.Sprintf("illegal transition %s -> %s", from, to))
		err.Details = map[string]any{"from": from, "to": to, "state_unchanged": from}
		e.lastError = err
		return err
	}
	e.states[roomID] = to
	if to != StateFailed {
		delete(e.stateErrors, roomID)
	}
	return nil
}

func (e *Engine) failRoomLocked(roomID string, err *model.Error) {
	current := e.states[roomID]
	if current == StateAnalyzing || current == StateSolving || current == StatePending {
		if current == StatePending {
			e.states[roomID] = StateAnalyzing
		}
		e.states[roomID] = StateFailed
	}
	if err != nil {
		e.stateErrors[roomID] = err.Error()
		e.lastError = err
	}
}

func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()

	order := CommitOrder(e.level, e.cfg)
	stateRooms := make([]StateRoom, 0, len(order))
	committed := 0
	for _, id := range order {
		state := e.states[id]
		if state == StateCommitted {
			committed++
		}
		stateRooms = append(stateRooms, StateRoom{
			ID:       id,
			State:    state,
			Progress: fmt.Sprintf("%d/%d", committed, len(order)),
			Revision: e.revisions[id],
			Error:    e.stateErrors[id],
		})
	}
	queueEvents := e.pool.RecordedEvents()
	levelCopy := CloneLevel(e.level)
	var resultCopy *model.FullResult
	if e.result != nil {
		data, _ := json.Marshal(e.result)
		var copied model.FullResult
		if err := json.Unmarshal(data, &copied); err == nil {
			resultCopy = &copied
		}
	}
	return Snapshot{
		Config:           e.cfg,
		Level:            levelCopy,
		States:           stateRooms,
		Committed:        committed,
		Total:            len(order),
		Progress:         fmt.Sprintf("%d/%d", committed, len(order)),
		Result:           resultCopy,
		LastPlan:         e.lastPlan,
		CacheStats:       e.stats,
		QueueEvents:      queueEvents,
		RecoveredRooms:   e.recoveredRooms,
		SnapshotInvalid:  e.snapshotInvalid,
		LastError:        e.lastError,
		CommitWriteOrder: append([]string(nil), e.commitWriteOrder...),
		UpdatedAt:        time.Now(),
	}
}

func (e *Engine) SetLastError(err *model.Error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastError = err
}
