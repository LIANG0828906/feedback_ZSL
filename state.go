package loudness

import (
	"fmt"
	"sort"
	"sync"
)

// ClipState is the five-state clip lifecycle.
type ClipState string

const (
	StatePendingLoad ClipState = "pending-load"
	StateMeasuring   ClipState = "measuring-envelope"
	StateCalibrating ClipState = "calibrating-gain"
	StateCommitted   ClipState = "committed"
	StateFailed      ClipState = "failed"
)

func (s ClipState) String() string { return string(s) }

type stateEntry struct {
	state    ClipState
	revision int64
	frozen   *ClipResult
}

// StateSnapshot is an immutable observation of one clip.
type StateSnapshot struct {
	ClipID   int         `json:"clipId"`
	State    ClipState   `json:"state"`
	Revision int64       `json:"revision"`
	Frozen   *ClipResult `json:"frozen,omitempty"`
}

// StateMachine enforces legal clip transitions and commit freeze semantics.
type StateMachine struct {
	mu        sync.Mutex
	entries   map[int]*stateEntry
	total     int
	committed int
}

func NewStateMachine(clipIDs []int) (*StateMachine, error) {
	machine := &StateMachine{entries: make(map[int]*stateEntry, len(clipIDs)), total: len(clipIDs)}
	for _, id := range clipIDs {
		if _, exists := machine.entries[id]; exists {
			return nil, &DomainError{Code: ESchema, Path: "$.clips", Field: "id", Detail: fmt.Sprintf("duplicate clip id %d", id)}
		}
		machine.entries[id] = &stateEntry{state: StatePendingLoad}
	}
	return machine, nil
}

func (m *StateMachine) Progress() (committed, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.committed, m.total
}

func legalTransition(from, to ClipState) bool {
	switch from {
	case StatePendingLoad:
		return to == StateMeasuring
	case StateMeasuring:
		return to == StateCalibrating || to == StateFailed
	case StateCalibrating:
		return to == StateCommitted || to == StateFailed
	default:
		return false
	}
}

func (m *StateMachine) stateError(entry *stateEntry, id int, from, to ClipState, detail string) error {
	return &DomainError{
		Code: EState, Path: fmt.Sprintf("$.clips[%d].state", id), Field: "state",
		FromState: from, ToState: to, CommitCount: m.committed, ClipTotal: m.total, Detail: detail,
	}
}

// Transition moves one clip if the transition is legal. On error, it does not
// mutate state, frozen values, or progress.
func (m *StateMachine) Transition(id int, to ClipState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok {
		return &DomainError{Code: EState, Path: fmt.Sprintf("$.clips[%d].state", id), Field: "state", ToState: to, Detail: "unknown clip"}
	}
	if !legalTransition(entry.state, to) {
		return m.stateError(entry, id, entry.state, to, "illegal transition")
	}
	entry.state = to
	entry.revision++
	return nil
}

// Commit performs Calibrating->Committed and freezes a deep copy of result.
func (m *StateMachine) Commit(id int, result ClipResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok {
		return &DomainError{Code: EState, Path: fmt.Sprintf("$.clips[%d].state", id), Field: "state", ToState: StateCommitted, Detail: "unknown clip"}
	}
	if !legalTransition(entry.state, StateCommitted) {
		return m.stateError(entry, id, entry.state, StateCommitted, "commit requires calibrating state")
	}
	entry.state = StateCommitted
	entry.revision++
	frozen := cloneClipResult(result)
	entry.frozen = &frozen
	m.committed++
	return nil
}

// Invalidate is the only path from Committed/Failed back to PendingLoad.
// It unfreezes the clip and never affects other clips.
func (m *StateMachine) Invalidate(id int, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok {
		return &DomainError{Code: EState, Path: fmt.Sprintf("$.clips[%d].state", id), Field: "state", Detail: "unknown clip"}
	}
	switch entry.state {
	case StatePendingLoad:
		return nil
	case StateCommitted, StateFailed:
		if entry.state == StateCommitted {
			m.committed--
		}
		entry.state = StatePendingLoad
		entry.frozen = nil
		entry.revision++
		return nil
	default:
		return m.stateError(entry, id, entry.state, StatePendingLoad, "cannot invalidate during "+reason)
	}
}

func (m *StateMachine) Snapshot(id int) (StateSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok {
		return StateSnapshot{}, false
	}
	snapshot := StateSnapshot{ClipID: id, State: entry.state, Revision: entry.revision}
	if entry.frozen != nil {
		frozen := cloneClipResult(*entry.frozen)
		snapshot.Frozen = &frozen
	}
	return snapshot, true
}

func (m *StateMachine) Snapshots() []StateSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]int, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	snapshots := make([]StateSnapshot, 0, len(ids))
	for _, id := range ids {
		entry := m.entries[id]
		snapshot := StateSnapshot{ClipID: id, State: entry.state, Revision: entry.revision}
		if entry.frozen != nil {
			frozen := cloneClipResult(*entry.frozen)
			snapshot.Frozen = &frozen
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

func cloneClipResult(result ClipResult) ClipResult {
	clone := result
	clone.Frames = make([]FrameEnvelope, len(result.Frames))
	for i, frame := range result.Frames {
		clone.Frames[i] = frame
		clone.Frames[i].Truncations = append([]Truncation(nil), frame.Truncations...)
	}
	clone.Truncations = append([]Truncation(nil), result.Truncations...)
	return clone
}
