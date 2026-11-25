package colortrace

import "sync"

var legalUnitTransitions = map[UnitState]map[UnitState]bool{
	StatePending:   {StateRunning: true},
	StateRunning:   {StateCommitted: true, StateFailed: true},
	StateFailed:    {StateRunning: true},
	StateCommitted: {StatePending: true},
}

// ChunkStateMachine guards stage/chunk unit transitions.
type ChunkStateMachine struct {
	mu     sync.Mutex
	states map[string]UnitState
}

func NewChunkStateMachine() *ChunkStateMachine {
	return &ChunkStateMachine{states: map[string]UnitState{}}
}

func (m *ChunkStateMachine) State(stage int, chunkID string) UnitState {
	key := UnitKey(stage, chunkID)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked(key)
}

// Transition performs a legal transition or returns E_STATE without mutation.
func (m *ChunkStateMachine) Transition(stage int, chunkID string, to UnitState) (UnitState, error) {
	key := UnitKey(stage, chunkID)
	m.mu.Lock()
	defer m.mu.Unlock()
	from := m.stateLocked(key)
	if from == to {
		return from, nil
	}
	if !legalUnitTransitions[from][to] {
		return from, stateError(stage, chunkID, from, to)
	}
	m.states[key] = to
	return to, nil
}

func (m *ChunkStateMachine) stateLocked(key string) UnitState {
	if state, ok := m.states[key]; ok {
		return state
	}
	return StatePending
}

var legalStageTransitions = map[UnitState]map[UnitState]bool{
	StatePending:   {StateRunning: true},
	StateRunning:   {StateCommitted: true, StateFailed: true},
	StateFailed:    {StateRunning: true},
	StateCommitted: {StatePending: true},
}

// StageStateMachine guards processing-level lifecycle transitions.
type StageStateMachine struct {
	mu     sync.Mutex
	states map[int]UnitState
}

func NewStageStateMachine() *StageStateMachine {
	return &StageStateMachine{states: map[int]UnitState{}}
}

func (m *StageStateMachine) State(stage int) UnitState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.states[stage]; ok {
		return state
	}
	return StatePending
}

func (m *StageStateMachine) Transition(stage int, to UnitState) (UnitState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	from := StatePending
	if state, ok := m.states[stage]; ok {
		from = state
	}
	if from == to {
		return from, nil
	}
	if !legalStageTransitions[from][to] {
		err := stateError(stage, "", from, to)
		return from, err
	}
	m.states[stage] = to
	return to, nil
}
