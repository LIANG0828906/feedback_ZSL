package core

import "sync"

type FrameState string

const (
	StatePending   FrameState = "pending"
	StateSolving   FrameState = "solving"
	StateCommitted FrameState = "committed"
	StateFailed    FrameState = "failed"
)

var legalTransitions = map[FrameState]map[FrameState]bool{
	StatePending:   {StateSolving: true},
	StateSolving:   {StateCommitted: true, StateFailed: true},
	StateCommitted: {StateSolving: true},
	StateFailed:    {StateSolving: true},
}

type StateMachine struct {
	mu     sync.Mutex
	states map[int]FrameState
}

func NewStateMachine(frameCount int) *StateMachine {
	sm := &StateMachine{states: make(map[int]FrameState, frameCount)}
	for i := 0; i < frameCount; i++ {
		sm.states[i] = StatePending
	}
	return sm
}

func (sm *StateMachine) State(frame int) FrameState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if s, ok := sm.states[frame]; ok {
		return s
	}
	return ""
}

func (sm *StateMachine) Transition(frame int, next FrameState) *Error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	current, ok := sm.states[frame]
	if !ok {
		current = StatePending
		sm.states[frame] = current
	}
	if current == next {
		return nil
	}
	if !legalTransitions[current][next] {
		return &Error{Code: ECodeState, Frame: frame, Field: "state", Message: "illegal state transition " + string(current) + " -> " + string(next), Details: map[string]string{"from": string(current), "to": string(next)}}
	}
	sm.states[frame] = next
	return nil
}

func (sm *StateMachine) Snapshot() map[int]FrameState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	out := make(map[int]FrameState, len(sm.states))
	for k, v := range sm.states {
		out[k] = v
	}
	return out
}
