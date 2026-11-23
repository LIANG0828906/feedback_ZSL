package engine

import (
	"sync"

	"loudnessworkbench/apperr"
)

type SegmentState string

const (
	StatePending    SegmentState = "pending"
	StateProcessing SegmentState = "processing"
	StateCommitted  SegmentState = "committed"
	StateFailed     SegmentState = "failed"
)

func LegalTransition(from, to SegmentState) bool {
	switch from {
	case StatePending:
		return to == StateProcessing
	case StateProcessing:
		return to == StateCommitted || to == StateFailed || to == StatePending
	case StateFailed:
		return to == StateProcessing || to == StatePending
	default:
		return false
	}
}

type stateMachine struct {
	mu    sync.Mutex
	index int
	id    string
	state SegmentState
}

func newStateMachine(index int, id string) *stateMachine {
	return &stateMachine{index: index, id: id, state: StatePending}
}

func (m *stateMachine) Current() SegmentState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *stateMachine) Transition(next SegmentState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !LegalTransition(m.state, next) {
		return apperr.New(apperr.CodeState, "illegal segment state transition").
			WithField("state").WithExpected(m.state).WithActual(next).
			WithSegment(m.index, m.id)
	}
	m.state = next
	return nil
}
