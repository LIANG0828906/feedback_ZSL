package core

import (
	"fmt"
	"sync"
)

type State string

const (
	StatePending    State = "pending"
	StateProcessing State = "processing"
	StateCommitted  State = "committed"
	StateFailed     State = "failed"
)

func ValidState(s State) bool {
	switch s {
	case StatePending, StateProcessing, StateCommitted, StateFailed:
		return true
	default:
		return false
	}
}

func TransitionAllowed(from, to State) bool {
	switch from {
	case StatePending:
		return to == StateProcessing
	case StateProcessing:
		return to == StateCommitted || to == StateFailed
	case StateFailed:
		return to == StatePending
	case StateCommitted:
		return to == StatePending
	default:
		return false
	}
}

type Machine struct {
	mu       sync.Mutex
	state    State
	step     int
	block    *BlockRef
	label    string
	onChange func(from, to State)
}

func NewMachine(initial State, step int, block *BlockRef, label string) *Machine {
	if !ValidState(initial) {
		initial = StatePending
	}
	var b *BlockRef
	if block != nil {
		cp := *block
		b = &cp
	}
	return &Machine{state: initial, step: step, block: b, label: label}
}

func (m *Machine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *Machine) SetOnChange(fn func(from, to State)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = fn
}

func (m *Machine) Transition(to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from := m.state
	if !TransitionAllowed(from, to) {
		return StateError(string(from), string(to), m.step, m.block,
			fmt.Sprintf("illegal %s transition", m.label))
	}
	m.state = to
	if m.onChange != nil {
		m.onChange(from, to)
	}
	return nil
}
