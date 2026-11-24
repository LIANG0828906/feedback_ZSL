package solver

import (
	"fmt"
	"sort"
	"sync"

	"levelacoustic/internal/model"
)

type StateMachine struct {
	mu         sync.RWMutex
	roomIDs    []string
	states     map[string]model.RoomState
	generation map[string]int
	reason     map[string]string
}

func NewStateMachine(roomIDs []string) *StateMachine {
	ids := append([]string(nil), roomIDs...)
	sort.Strings(ids)
	m := &StateMachine{
		roomIDs:    ids,
		states:     make(map[string]model.RoomState, len(ids)),
		generation: make(map[string]int, len(ids)),
		reason:     make(map[string]string, len(ids)),
	}
	for _, id := range ids {
		m.states[id] = model.StatePending
	}
	return m
}

func (m *StateMachine) Transition(roomID string, to model.RoomState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, ok := m.states[roomID]
	if !ok {
		return &model.Error{Code: model.ErrState, Message: fmt.Sprintf("unknown room %s", roomID), RoomID: roomID}
	}
	if !model.LegalTransition(from, to) {
		return &model.Error{
			Code:     model.ErrState,
			Message:  fmt.Sprintf("illegal room transition %s -> %s", from, to),
			RoomID:   roomID,
			Expected: from,
			Actual:   to,
		}
	}
	m.states[roomID] = to
	m.generation[roomID]++
	return nil
}

// InvalidateForRevision is intentionally separate from Transition. It models a
// committed snapshot being frozen into a prior revision and a new work item
// being opened; normal state-machine calls still reject committed -> pending.
func (m *StateMachine) InvalidateForRevision(roomID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.states[roomID]; !ok {
		return &model.Error{Code: model.ErrState, Message: fmt.Sprintf("unknown room %s", roomID), RoomID: roomID}
	}
	m.states[roomID] = model.StatePending
	m.generation[roomID]++
	m.reason[roomID] = reason
	return nil
}

func (m *StateMachine) Restore(roomID string, state model.RoomState, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.states[roomID]; !ok {
		return &model.Error{Code: model.ErrState, Message: fmt.Sprintf("unknown room %s", roomID), RoomID: roomID}
	}
	m.states[roomID] = state
	m.reason[roomID] = reason
	return nil
}

func (m *StateMachine) State(roomID string) model.RoomState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.states[roomID]
}

func (m *StateMachine) Snapshot() []model.RoomStateSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]model.RoomStateSnapshot, 0, len(m.roomIDs))
	for _, id := range m.roomIDs {
		out = append(out, model.RoomStateSnapshot{
			RoomID:     id,
			State:      m.states[id],
			Changes:    m.generation[id],
			LastReason: m.reason[id],
		})
	}
	return out
}

func (m *StateMachine) CommittedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, state := range m.states {
		if state == model.StateCommitted {
			n++
		}
	}
	return n
}

func (m *StateMachine) Progress() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, state := range m.states {
		if state == model.StateCommitted {
			n++
		}
	}
	return fmt.Sprintf("%d/%d", n, len(m.roomIDs))
}

func (m *StateMachine) ResetFailed(roomID string) error {
	if err := m.Transition(roomID, model.StatePending); err != nil {
		return err
	}
	return nil
}

func (m *StateMachine) Fail(roomID, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[roomID]
	if !ok {
		return
	}
	if model.LegalTransition(state, model.StateFailed) {
		m.states[roomID] = model.StateFailed
		m.generation[roomID]++
		m.reason[roomID] = reason
	}
}
