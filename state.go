package lightshow

import "sync"

type WindowState string

const (
	StatePending    WindowState = "pending"
	StateScheduling WindowState = "scheduling"
	StateCommitted  WindowState = "committed"
	StateFailed     WindowState = "failed"
)

func legalTransition(from, to WindowState) bool {
	switch from {
	case StatePending:
		return to == StateScheduling
	case StateScheduling:
		return to == StateCommitted || to == StateFailed
	case StateFailed:
		return to == StateScheduling
	default:
		return false
	}
}

// WindowFSM is a small in-memory reference state machine. The SQLite store
// applies the same transition table transactionally.
type WindowFSM struct {
	mu    sync.Mutex
	state WindowState
}

func NewWindowFSM() *WindowFSM {
	return &WindowFSM{state: StatePending}
}

func (m *WindowFSM) State() WindowState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *WindowFSM) Transition(to WindowState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !legalTransition(m.state, to) {
		return engineError(CodeState, "window", "illegal window state transition %s -> %s", m.state, to)
	}
	m.state = to
	return nil
}
