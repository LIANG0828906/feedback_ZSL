package checkpoint

import (
	"fmt"
	"sync"

	"mocapworkbench/internal/model"
)

type FrameState string

const (
	StatePending   FrameState = "pending"
	StateSolving   FrameState = "solving"
	StateCommitted FrameState = "committed"
	StateFailed    FrameState = "failed"
)

type StateMachine struct {
	mu     sync.Mutex
	states []FrameState
}

func NewStateMachine(frameCount int) *StateMachine {
	states := make([]FrameState, frameCount)
	for i := range states {
		states[i] = StatePending
	}
	return &StateMachine{states: states}
}

func (s *StateMachine) State(frame int) (FrameState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if frame < 0 || frame >= len(s.states) {
		return "", model.FrameError(model.ErrState, "frame", frame, "state", "frame is out of range")
	}
	return s.states[frame], nil
}

func (s *StateMachine) Transition(frame int, next FrameState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if frame < 0 || frame >= len(s.states) {
		return model.FrameError(model.ErrState, "frame", frame, "state", "frame is out of range")
	}
	current := s.states[frame]
	if !validTransition(current, next) {
		return model.FrameError(model.ErrState, "frame", frame, "state", fmt.Sprintf("illegal transition %s -> %s; state preserved as %s", current, next, current))
	}
	s.states[frame] = next
	return nil
}

func (s *StateMachine) MarkCommittedPrefix(count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if count < 0 {
		count = 0
	}
	if count > len(s.states) {
		count = len(s.states)
	}
	for i := 0; i < count; i++ {
		s.states[i] = StateCommitted
	}
}

func (s *StateMachine) Snapshot() []FrameState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]FrameState(nil), s.states...)
}

func validTransition(current, next FrameState) bool {
	switch current {
	case StatePending:
		return next == StateSolving
	case StateSolving:
		return next == StateCommitted || next == StateFailed
	case StateFailed:
		return next == StatePending
	case StateCommitted:
		return false
	default:
		return false
	}
}
