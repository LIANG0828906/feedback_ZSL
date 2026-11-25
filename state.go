package vectorcore

import (
	"fmt"
	"sync"
)

type Stage string

const (
	StageFlatten   Stage = "flatten"
	StageCoverage  Stage = "coverage"
	StageComposite Stage = "composite"
)

func (s Stage) valid() bool {
	return s == StageFlatten || s == StageCoverage || s == StageComposite
}

type StageState string

const (
	StatePending    StageState = "pending"
	StateProcessing StageState = "processing"
	StateCommitted  StageState = "committed"
	StateFailed     StageState = "failed"
)

func legalTransition(from, to StageState) bool {
	switch from {
	case StatePending:
		return to == StateProcessing
	case StateProcessing:
		return to == StateCommitted || to == StateFailed
	case StateFailed:
		return to == StatePending || to == StateProcessing
	case StateCommitted:
		return false
	default:
		return false
	}
}

type stageKey struct {
	timeIndex int
	pathID    string
	stage     Stage
}

type stateMachine struct {
	mu     sync.Mutex
	states map[stageKey]StageState
}

func newStateMachine() *stateMachine {
	return &stateMachine{states: make(map[stageKey]StageState)}
}

func (m *stateMachine) init(timeIndex int, pathID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
		k := stageKey{timeIndex: timeIndex, pathID: pathID, stage: stage}
		if _, ok := m.states[k]; !ok {
			m.states[k] = StatePending
		}
	}
}

func (m *stateMachine) get(timeIndex int, pathID string, stage Stage) StageState {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[stageKey{timeIndex: timeIndex, pathID: pathID, stage: stage}]
	if !ok {
		return StatePending
	}
	return s
}

func (m *stateMachine) transition(timeIndex int, pathID string, stage Stage, to StageState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !stage.valid() {
		return &Error{Code: CodeState, Op: "transition", Stage: stage, PathID: pathID, TimeIndex: timeIndex, Keyframe: -1,
			Err: fmt.Errorf("unknown stage")}
	}
	k := stageKey{timeIndex: timeIndex, pathID: pathID, stage: stage}
	from, ok := m.states[k]
	if !ok {
		from = StatePending
		m.states[k] = from
	}
	if !legalTransition(from, to) {
		return &Error{Code: CodeState, Op: "transition", Stage: stage, PathID: pathID, TimeIndex: timeIndex, Keyframe: -1,
			Err: fmt.Errorf("illegal transition %s -> %s", from, to)}
	}
	m.states[k] = to
	return nil
}

func (m *stateMachine) resetFailed(timeIndex int, pathID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
		k := stageKey{timeIndex: timeIndex, pathID: pathID, stage: stage}
		if m.states[k] == StateFailed {
			m.states[k] = StatePending
		}
	}
}

// resetInvalidated clears stage bookkeeping for an *uncommitted* sample time
// after its cache entries were invalidated. Whole-time commits remain terminal;
// this internal path is not exposed through Transition.
func (m *stateMachine) resetInvalidated(timeIndex int, pathID string, stages map[Stage]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
		if stages[stage] {
			m.states[stageKey{timeIndex: timeIndex, pathID: pathID, stage: stage}] = StatePending
		}
	}
}

func (m *stateMachine) freezeTime(timeIndex int, pathIDs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pathID := range pathIDs {
		for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
			k := stageKey{timeIndex: timeIndex, pathID: pathID, stage: stage}
			m.states[k] = StateCommitted
		}
	}
}

func (m *stateMachine) statesForTime(timeIndex int, pathIDs []string) map[Stage]map[string]StageState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[Stage]map[string]StageState, 3)
	for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
		out[stage] = make(map[string]StageState, len(pathIDs))
		for _, pathID := range pathIDs {
			s, ok := m.states[stageKey{timeIndex: timeIndex, pathID: pathID, stage: stage}]
			if !ok {
				s = StatePending
			}
			out[stage][pathID] = s
		}
	}
	return out
}
