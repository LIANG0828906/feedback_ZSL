package statemachine

import (
	"fmt"

	"terrain-occlusion/internal/model"
)

const (
	Pending   = "pending"
	Culling   = "culling"
	Loaded    = "loaded"
	Committed = "committed"
	Failed    = "failed"
)

var allowed = map[string]map[string]bool{
	Pending:   {Culling: true},
	Culling:   {Loaded: true, Failed: true},
	Loaded:    {Committed: true, Failed: true},
	Committed: {},
	Failed:    {},
}

type Machine struct {
	States    []string
	Committed int
}

func New(total int) *Machine {
	m := &Machine{States: make([]string, total), Committed: 0}
	for i := range m.States {
		m.States[i] = Pending
	}
	return m
}

func (m *Machine) Transition(index int, to string) error {
	if index < 0 || index >= len(m.States) {
		return model.NewError(model.EState, "sample_index", "index %d outside 0..%d", index, len(m.States)-1)
	}
	from := m.States[index]
	if !allowed[from][to] {
		return &model.Error{
			Code:    model.EState,
			Field:   fmt.Sprintf("samples[%d].state", index),
			Message: fmt.Sprintf("illegal transition %s -> %s", from, to),
			Details: map[string]any{"sample_index": index, "from": from, "to": to},
		}
	}
	m.States[index] = to
	if to == Committed {
		m.Committed++
	}
	return nil
}

func (m *Machine) State(index int) string {
	if index < 0 || index >= len(m.States) {
		return ""
	}
	return m.States[index]
}

func (m *Machine) Progress() (committed, total int) {
	return m.Committed, len(m.States)
}
