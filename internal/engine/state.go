package engine

import (
	"sync"

	"ms-workbench/internal/apperr"
)

type SegmentState string

const (
	Pending    SegmentState = "pending"
	Processing SegmentState = "processing"
	Committed  SegmentState = "committed"
	Failed     SegmentState = "failed"
)

var legalTransitions = map[SegmentState]map[SegmentState]bool{
	Pending: {
		Processing: true,
	},
	Processing: {
		Committed: true,
		Failed:    true,
		Pending:   true, // pause/rollback boundary
	},
	Failed: {
		Processing: true, // explicit retry
		Pending:    true, // explicit dependency invalidation
	},
	Committed: {
		Pending: true, // explicit dependency invalidation
	},
}

func CanTransition(from, to SegmentState) bool {
	return legalTransitions[from][to]
}

// StateMachine is an observable four-state segment state machine. Illegal
// transitions leave the original state untouched.
type StateMachine struct {
	mu    sync.Mutex
	state SegmentState
}

func NewStateMachine(initial SegmentState) *StateMachine {
	return &StateMachine{state: initial}
}

func (m *StateMachine) State() SegmentState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *StateMachine) Transition(next SegmentState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return transitionLocked(&m.state, next, -1)
}

func transitionLocked(state *SegmentState, next SegmentState, segmentID int) error {
	from := *state
	if !CanTransition(from, next) {
		return &apperr.Error{
			Code:      apperr.ESTATE,
			Message:   "illegal segment state transition",
			Segment:   segmentID,
			FromState: string(from),
			ToState:   string(next),
		}
	}
	*state = next
	return nil
}
