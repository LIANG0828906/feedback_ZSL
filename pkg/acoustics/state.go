package acoustics

import "sync"

type State string

const (
	StatePending    State = "pending"
	StateConnecting State = "connecting"
	StateReflecting State = "reflecting"
	StateCommitted  State = "committed"
	StateFailed     State = "failed"
)

var legalTransitions = map[State]map[State]bool{
	StatePending:    {StateConnecting: true, StateFailed: true},
	StateConnecting: {StateReflecting: true, StateFailed: true},
	StateReflecting: {StateCommitted: true, StateFailed: true},
	StateFailed:     {StatePending: true, StateConnecting: true},
	StateCommitted:  {StatePending: true},
}

// StateMachine guards per-room chunk lifecycle transitions.
type StateMachine struct {
	mu     sync.RWMutex
	states map[string]State
	order  []string
}

func NewStateMachine(roomIDs []string) *StateMachine {
	ids := append([]string{}, roomIDs...)
	states := make(map[string]State, len(ids))
	for _, id := range ids {
		states[id] = StatePending
	}
	return &StateMachine{states: states, order: ids}
}

func (m *StateMachine) State(roomID string) (State, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.states[roomID]
	return s, ok
}

// Transition validates and performs a legal transition. Illegal calls return
// E_STATE and leave the current state untouched.
func (m *StateMachine) Transition(roomID string, to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, ok := m.states[roomID]
	if !ok {
		return Errorf(CodeState, roomID, "state", "unknown room")
	}
	if from == to {
		return nil
	}
	if !legalTransitions[from][to] {
		return &DomainError{Code: CodeState, ObjectID: roomID, Field: "state", Message: "illegal transition " + string(from) + " -> " + string(to), Details: map[string]string{"current": string(from), "requested": string(to)}}
	}
	m.states[roomID] = to
	return nil
}

// Restore sets a recovered state without enforcing a fresh-process transition
// history. Checkpoint loading has already validated persistence/version.
func (m *StateMachine) Restore(roomID string, state State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.states[roomID]; ok {
		m.states[roomID] = state
	}
}

func (m *StateMachine) Snapshot() map[string]State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]State, len(m.states))
	for k, v := range m.states {
		out[k] = v
	}
	return out
}

type Progress struct {
	Committed int `json:"committed_rooms"`
	Total     int `json:"total_rooms"`
	Failed    int `json:"failed_rooms"`
}

func (m *StateMachine) Progress() Progress {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := Progress{Total: len(m.order)}
	for _, s := range m.states {
		if s == StateCommitted {
			p.Committed++
		}
		if s == StateFailed {
			p.Failed++
		}
	}
	return p
}
