package acoustic

import (
	"fmt"
	"sync"
)

type RoomState string

const (
	RoomPending           RoomState = "pending"
	RoomAnalyzingChain    RoomState = "analyzing_chain"
	RoomSolvingReflection RoomState = "solving_reflection"
	RoomCommitted         RoomState = "committed"
	RoomFailed            RoomState = "failed"
)

type RoomStatus struct {
	RoomID         int       `json:"roomId"`
	State          RoomState `json:"state"`
	CommitSequence int       `json:"commitSequence,omitempty"`
	Failure        string    `json:"failure,omitempty"`
}

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

type StateMachine struct {
	mu        sync.RWMutex
	states    map[int]RoomState
	failures  map[int]string
	sequences map[int]int
	total     int
	committed int
}

func NewStateMachine(roomIDs []int) (*StateMachine, error) {
	if len(roomIDs) == 0 {
		return nil, &Error{Code: CodeSchema, Op: "state", Message: "at least one room is required"}
	}
	machine := &StateMachine{
		states:    make(map[int]RoomState, len(roomIDs)),
		failures:  make(map[int]string),
		sequences: make(map[int]int),
		total:     len(roomIDs),
	}
	for _, roomID := range roomIDs {
		if roomID <= 0 {
			return nil, &Error{Code: CodeSchema, Op: "state", RoomIDs: []int{roomID}, Message: "room id must be positive"}
		}
		if _, exists := machine.states[roomID]; exists {
			return nil, &Error{Code: CodeSchema, Op: "state", RoomIDs: []int{roomID}, Message: "duplicate room id"}
		}
		machine.states[roomID] = RoomPending
	}
	return machine, nil
}

func (m *StateMachine) Transition(roomID int, target RoomState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.states[roomID]
	if !ok {
		return &Error{Code: CodeState, Op: "transition", RoomIDs: []int{roomID}, Message: "unknown room"}
	}
	if !legalTransition(current, target) {
		return &Error{
			Code:    CodeState,
			Op:      "transition",
			RoomIDs: []int{roomID},
			Message: fmt.Sprintf("illegal transition %s -> %s", current, target),
		}
	}
	if current == RoomCommitted {
		return &Error{Code: CodeState, Op: "transition", RoomIDs: []int{roomID}, Message: "committed room is frozen"}
	}
	if current == RoomFailed && target == RoomPending {
		delete(m.failures, roomID)
	}
	if target == RoomCommitted {
		m.committed++
	}
	m.states[roomID] = target
	return nil
}

func (m *StateMachine) Fail(roomID int, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.states[roomID]
	if !ok {
		return &Error{Code: CodeState, Op: "fail", RoomIDs: []int{roomID}, Message: "unknown room"}
	}
	if current == RoomCommitted {
		return &Error{Code: CodeState, Op: "fail", RoomIDs: []int{roomID}, Message: "committed room cannot fail"}
	}
	if current == RoomFailed {
		m.failures[roomID] = reason
		return nil
	}
	if current == RoomPending {
		m.states[roomID] = RoomFailed
		m.failures[roomID] = reason
		return nil
	}
	m.states[roomID] = RoomFailed
	m.failures[roomID] = reason
	return nil
}

func (m *StateMachine) RestoreCommitted(roomID, sequence int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.states[roomID]
	if !ok {
		return &Error{Code: CodeState, Op: "restore", RoomIDs: []int{roomID}, Message: "unknown room"}
	}
	if current != RoomPending {
		return &Error{Code: CodeState, Op: "restore", RoomIDs: []int{roomID}, Message: "restored room must still be pending"}
	}
	m.states[roomID] = RoomCommitted
	m.sequences[roomID] = sequence
	m.committed++
	return nil
}

func (m *StateMachine) ResetPending(roomID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states[roomID] == RoomCommitted || m.states[roomID] == RoomPending {
		return
	}
	m.states[roomID] = RoomPending
	delete(m.failures, roomID)
}

func (m *StateMachine) State(roomID int) (RoomStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, ok := m.states[roomID]
	if !ok {
		return RoomStatus{}, false
	}
	return RoomStatus{RoomID: roomID, State: state, CommitSequence: m.sequences[roomID], Failure: m.failures[roomID]}, true
}

func (m *StateMachine) Progress() Progress {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Progress{Committed: m.committed, Total: m.total}
}

func (m *StateMachine) All() []RoomStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	statuses := make([]RoomStatus, 0, len(m.states))
	for roomID, state := range m.states {
		statuses = append(statuses, RoomStatus{
			RoomID:         roomID,
			State:          state,
			CommitSequence: m.sequences[roomID],
			Failure:        m.failures[roomID],
		})
	}
	sortRoomStatuses(statuses)
	return statuses
}

func sortRoomStatuses(values []RoomStatus) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j].RoomID < values[j-1].RoomID; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func legalTransition(from, to RoomState) bool {
	switch from {
	case RoomPending:
		return to == RoomAnalyzingChain || to == RoomFailed
	case RoomAnalyzingChain:
		return to == RoomSolvingReflection || to == RoomFailed
	case RoomSolvingReflection:
		return to == RoomCommitted || to == RoomFailed
	case RoomFailed:
		return to == RoomPending
	case RoomCommitted:
		return false
	default:
		return false
	}
}
