package skillcore

import (
	"fmt"
	"sync"
)

type WindowState string

const (
	WindowPending   WindowState = "pending"
	WindowSettling  WindowState = "settling"
	WindowCommitted WindowState = "committed"
	WindowFailed    WindowState = "failed"
)

func CanTransitionWindow(from, to WindowState) bool {
	switch from {
	case WindowPending:
		return to == WindowSettling
	case WindowSettling:
		return to == WindowCommitted || to == WindowFailed
	case WindowFailed:
		return to == WindowSettling
	default:
		return false
	}
}

type WindowFSM struct {
	mu     sync.Mutex
	states map[int]WindowState
}

func NewWindowFSM(windowCount int) (*WindowFSM, error) {
	if windowCount < 0 {
		return nil, domainErr(CodeInput, "windowCount", "must be non-negative")
	}
	states := make(map[int]WindowState, windowCount)
	for i := 0; i < windowCount; i++ {
		states[i] = WindowPending
	}
	return &WindowFSM{states: states}, nil
}

func (fsm *WindowFSM) State(window int) (WindowState, error) {
	fsm.mu.Lock()
	defer fsm.mu.Unlock()
	state, ok := fsm.states[window]
	if !ok {
		return "", domainErr(CodeState, fmt.Sprintf("windows[%d]", window), "unknown window")
	}
	return state, nil
}

func (fsm *WindowFSM) Transition(window int, next WindowState) error {
	fsm.mu.Lock()
	defer fsm.mu.Unlock()
	current, ok := fsm.states[window]
	if !ok {
		return &Error{
			Code:    CodeState,
			Path:    fmt.Sprintf("windows[%d]", window),
			Message: "unknown window",
		}
	}
	if !CanTransitionWindow(current, next) {
		return &Error{
			Code:         CodeState,
			Path:         fmt.Sprintf("windows[%d]", window),
			Message:      fmt.Sprintf("illegal window transition %s -> %s", current, next),
			CurrentState: string(current),
			NextState:    string(next),
		}
	}
	fsm.states[window] = next
	return nil
}

func (fsm *WindowFSM) Snapshot() map[int]WindowState {
	fsm.mu.Lock()
	defer fsm.mu.Unlock()
	out := make(map[int]WindowState, len(fsm.states))
	for index, state := range fsm.states {
		out[index] = state
	}
	return out
}
