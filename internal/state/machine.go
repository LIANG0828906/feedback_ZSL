package state

import (
	"fmt"
	"sync"

	"skeletonretarget/internal/model"
)

type State string

const (
	PendingLoad     State = "pending_load"
	PendingRetarget State = "pending_retarget"
	Retargeting     State = "retargeting"
	Committed       State = "committed"
	Failed          State = "failed"
)

var legal = map[State]map[State]bool{
	PendingLoad: {
		PendingRetarget: true,
		Failed:          true,
	},
	PendingRetarget: {
		Retargeting: true,
	},
	Retargeting: {
		Committed: true,
		Failed:    true,
	},
	Committed: {
		PendingRetarget: true,
	},
	Failed: {
		PendingRetarget: true,
	},
}

type Snapshot struct {
	State    State
	Progress int
}

type Machine struct {
	mu       sync.Mutex
	state    State
	progress int
}

func New(initial State, progress int) *Machine {
	if _, ok := legal[initial]; !ok {
		initial = PendingLoad
	}
	return &Machine{state: initial, progress: progress}
}

// Transition never mutates state on an illegal edge. The supplied error names
// both states so a caller can preserve progress and report E_STATE.
func (m *Machine) Transition(next State, progress int) *model.Error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !legal[m.state][next] {
		return &model.Error{
			Code: model.CodeState, Frame: progress,
			Message: fmt.Sprintf("illegal interval transition %s -> %s (progress unchanged at %d)", m.state, next, m.progress),
		}
	}
	m.state = next
	m.progress = progress
	return nil
}

func (m *Machine) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{State: m.state, Progress: m.progress}
}

func Legal(from, to State) bool { return legal[from][to] }
