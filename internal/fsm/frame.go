package fsm

import (
	"fmt"

	"retargetlab/internal/model"
)

type FrameState string

const (
	Pending   FrameState = "pending"
	Solving   FrameState = "solving"
	Committed FrameState = "committed"
	Failed    FrameState = "failed"
)

var allowed = map[FrameState]map[FrameState]bool{
	Pending:   {Solving: true},
	Solving:   {Committed: true, Failed: true},
	Committed: {},
	Failed:    {Solving: true},
}

type Machine struct {
	state FrameState
}

func New() *Machine {
	return &Machine{state: Pending}
}

func (m *Machine) State() FrameState { return m.state }

func (m *Machine) CanTransition(to FrameState) bool {
	return allowed[m.state][to]
}

func (m *Machine) Transition(to FrameState) error {
	if !m.CanTransition(to) {
		return model.Err(model.E_STATE, "transition_frame",
			fmt.Sprintf("illegal transition %s -> %s", m.state, to))
	}
	m.state = to
	return nil
}

// Force is reserved for constructing an engine from already validated
// checkpoints. It is not part of the normal transition path.
func (m *Machine) Force(state FrameState) {
	m.state = state
}
