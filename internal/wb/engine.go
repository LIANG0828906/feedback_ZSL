package wb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
)

type Engine struct {
	mu        sync.RWMutex
	sample    *Sample
	store     CheckpointStore
	sched     *Scheduler
	revision  int
	inputHash string

	states      map[string]*RoomState
	results     map[string]ListenerResult
	checkpoints map[string]CheckpointData
	failRooms   map[string]bool

	lastInvalidation *InvalidationReport
	lastDiff         *DiffReport
	recovery         RecoveryReport
}

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

type StateView struct {
	Sample            *Sample              `json:"sample"`
	InputHash         string               `json:"inputHash"`
	Revision          int                  `json:"revision"`
	States            []RoomState          `json:"states"`
	Results           []ListenerResult     `json:"results"`
	Progress          Progress             `json:"progress"`
	CommitSequence    []string             `json:"commitSequence"`
	LastInvalidation  *InvalidationReport  `json:"lastInvalidation,omitempty"`
	LastDiff          *DiffReport          `json:"lastDiff,omitempty"`
	Recovery          RecoveryReport       `json:"recovery"`
	Scheduler         SchedulerDiagnostics `json:"scheduler"`
	Dependencies      map[string][]string  `json:"dependencies"`
	FailureInjections []string             `json:"failureInjections"`
}

func NewEngine(sample *Sample, store CheckpointStore) (*Engine, *APIError) {
	if apiErr := ValidateSample(sample); apiErr != nil {
		return nil, apiErr
	}
	e := &Engine{
		sample:      sample,
		store:       store,
		sched:       NewScheduler(sample.Precision),
		states:      map[string]*RoomState{},
		results:     map[string]ListenerResult{},
		checkpoints: map[string]CheckpointData{},
		failRooms:   map[string]bool{},
		revision:    0,
		inputHash:   InputFingerprint(sample),
	}
	for i, room := range e.sortedRoomsLocked() {
		e.states[room.ID] = &RoomState{
			RoomID:           room.ID,
			Status:           StatusPending,
			CommitOrder:      i + 1,
			ParamVersion:     ParamVersion,
			AlgorithmVersion: AlgorithmVersion,
		}
	}
	return e, nil
}

func (e *Engine) sortedRoomsLocked() []Room {
	rooms := append([]Room(nil), e.sample.Rooms...)
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].ID < rooms[j].ID })
	return rooms
}

func (e *Engine) SortedRoomIDs() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	rooms := e.sortedRoomsLocked()
	ids := make([]string, 0, len(rooms))
	for _, room := range rooms {
		ids = append(ids, room.ID)
	}
	return ids
}

var allowedTransitions = map[RoomStatus]map[RoomStatus]bool{
	StatusPending: {
		StatusAnalyzing: true,
		StatusFailed:    true,
	},
	StatusAnalyzing: {
		StatusSolving: true,
		StatusFailed:  true,
	},
	StatusSolving: {
		StatusCommitted: true,
		StatusFailed:    true,
	},
	StatusCommitted: {
		StatusPending: true,
	},
	StatusFailed: {
		StatusPending: true,
	},
}

func (e *Engine) TransitionRoom(roomID string, to RoomStatus) *APIError {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.transitionLocked(roomID, to)
}

func (e *Engine) transitionLocked(roomID string, to RoomStatus) *APIError {
	state, ok := e.states[roomID]
	if !ok {
		return NewAPIError(CodeSchema, roomID, "roomId", "unknown room")
	}
	if state.Status == to {
		return nil
	}
	if !allowedTransitions[state.Status][to] {
		return NewAPIError(CodeState, roomID, "status",
			"illegal room transition %s -> %s", state.Status, to)
	}
	state.Status = to
	if to == StatusPending {
		state.LastError = nil
	}
	return nil
}

func (e *Engine) SetFailureInjection(roomID string, enabled bool) *APIError {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.states[roomID]; !ok {
		return NewAPIError(CodeSchema, roomID, "roomId", "unknown room")
	}
	if enabled {
		e.failRooms[roomID] = true
	} else {
		delete(e.failRooms, roomID)
	}
	return nil
}

func (e *Engine) View() StateView {
	e.mu.RLock()
	defer e.mu.RUnlock()

	states := make([]RoomState, 0, len(e.states))
	committed := 0
	for _, room := range e.sortedRoomsLocked() {
		state := *e.states[room.ID]
		if state.LastError != nil {
			errCopy := *state.LastError
			state.LastError = &errCopy
		}
		states = append(states, state)
		if state.Status == StatusCommitted {
			committed++
		}
	}
	results := make([]ListenerResult, 0, len(e.results))
	for _, listener := range e.sample.Listeners {
		if result, ok := e.results[listener.ID]; ok {
			results = append(results, cloneListenerResult(result))
		}
	}
	failures := sortedKeys(e.failRooms)
	deps := e.buildDependenciesLocked()

	return StateView{
		Sample:            CloneSample(e.sample),
		InputHash:         InputFingerprint(e.sample),
		Revision:          e.revision,
		States:            states,
		Results:           results,
		Progress:          Progress{Committed: committed, Total: len(e.states)},
		CommitSequence:    e.commitSequenceLocked(),
		LastInvalidation:  cloneInvalidation(e.lastInvalidation),
		LastDiff:          cloneDiff(e.lastDiff),
		Recovery:          e.recovery,
		Scheduler:         e.sched.Diagnostics(),
		Dependencies:      deps,
		FailureInjections: failures,
	}
}

func (e *Engine) commitSequenceLocked() []string {
	states := make([]*RoomState, 0, len(e.states))
	for _, state := range e.states {
		if state.Status == StatusCommitted {
			states = append(states, state)
		}
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].CommitOrder != states[j].CommitOrder {
			return states[i].CommitOrder < states[j].CommitOrder
		}
		return states[i].RoomID < states[j].RoomID
	})
	ids := make([]string, 0, len(states))
	for _, state := range states {
		ids = append(ids, state.RoomID)
	}
	return ids
}

func InputFingerprint(s *Sample) string {
	fp := struct {
		Seed      int64         `json:"seed"`
		Bounds    Rect          `json:"bounds"`
		Precision PrecisionSpec `json:"precision"`
		Rooms     []Room        `json:"rooms"`
		Walls     []Wall        `json:"walls"`
		Openings  []Opening     `json:"openings"`
		Sources   []Source      `json:"sources"`
		Listeners []Listener    `json:"listeners"`
	}{s.Seed, s.Bounds, s.Precision, s.Rooms, s.Walls, s.Openings, s.Sources, s.Listeners}
	b, _ := json.Marshal(fp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func CloneSample(s *Sample) *Sample {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var out Sample
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

func cloneListenerResult(in ListenerResult) ListenerResult {
	b, _ := json.Marshal(in)
	var out ListenerResult
	_ = json.Unmarshal(b, &out)
	return out
}

func cloneInvalidation(in *InvalidationReport) *InvalidationReport {
	if in == nil {
		return nil
	}
	b, _ := json.Marshal(in)
	var out InvalidationReport
	_ = json.Unmarshal(b, &out)
	return &out
}

func cloneDiff(in *DiffReport) *DiffReport {
	if in == nil {
		return nil
	}
	b, _ := json.Marshal(in)
	var out DiffReport
	_ = json.Unmarshal(b, &out)
	return &out
}

func (e *Engine) Recover(ctx context.Context) *APIError {
	if e.store == nil {
		e.recovery = RecoveryReport{Reason: "no checkpoint store"}
		return nil
	}
	bundle, err := e.store.LoadCheckpoints(ctx)
	if err != nil {
		return NewAPIError(CodeInternal, "sqlite", "", "load checkpoints: %v", err)
	}
	if len(bundle.Items) == 0 {
		e.recovery = RecoveryReport{SampleFound: len(bundle.SampleJSON) > 0}
		return nil
	}
	if len(bundle.SampleJSON) > 0 {
		recoveredSample, apiErr := LoadSample(bundle.SampleJSON)
		if apiErr != nil {
			_ = e.store.ResetCheckpoints(ctx)
			e.recovery = RecoveryReport{Discarded: true, Reason: "stored sample failed validation: " + apiErr.Error()}
			return nil
		}
		if InputFingerprint(recoveredSample) != bundle.InputHash {
			_ = e.store.ResetCheckpoints(ctx)
			e.recovery = RecoveryReport{Discarded: true, Reason: "stored sample input hash mismatch"}
			return nil
		}
		e.sample = recoveredSample
		e.sched = NewScheduler(recoveredSample.Precision)
		e.states = map[string]*RoomState{}
		e.results = map[string]ListenerResult{}
		e.checkpoints = map[string]CheckpointData{}
		for i, room := range e.sortedRoomsLocked() {
			e.states[room.ID] = &RoomState{
				RoomID: room.ID, Status: StatusPending, CommitOrder: i + 1,
				ParamVersion: ParamVersion, AlgorithmVersion: AlgorithmVersion,
			}
		}
	}

	validItems := make([]StoredCheckpoint, 0, len(bundle.Items))
	for _, item := range bundle.Items {
		if _, apiErr := VerifyAndDecodeStoredCheckpoint(item, bundle.InputHash); apiErr != nil {
			_ = e.store.ResetCheckpoints(ctx)
			e.results = map[string]ListenerResult{}
			e.checkpoints = map[string]CheckpointData{}
			for id := range e.states {
				order := e.states[id].CommitOrder
				e.states[id] = &RoomState{
					RoomID: id, Status: StatusPending, CommitOrder: order,
					ParamVersion: ParamVersion, AlgorithmVersion: AlgorithmVersion,
				}
			}
			e.recovery = RecoveryReport{Discarded: true, Reason: apiErr.Error()}
			return nil
		}
		validItems = append(validItems, item)
	}

	sort.Slice(validItems, func(i, j int) bool { return validItems[i].CommitOrder < validItems[j].CommitOrder })
	for _, item := range validItems {
		data, _ := VerifyAndDecodeStoredCheckpoint(item, bundle.InputHash)
		e.checkpoints[item.RoomID] = *data
		for _, result := range data.ListenerResults {
			e.results[result.ListenerID] = result
		}
		state := e.states[item.RoomID]
		if state == nil {
			continue
		}
		state.Status = StatusCommitted
		state.ContentHash = item.ContentHash
		state.ParamVersion = item.ParamVersion
		state.AlgorithmVersion = item.AlgorithmVersion
		state.LastError = nil
	}
	e.inputHash = bundle.InputHash
	e.revision = bundle.Revision
	e.recovery = RecoveryReport{Recovered: len(validItems), SampleFound: len(bundle.SampleJSON) > 0}
	return nil
}
