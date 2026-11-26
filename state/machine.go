package state

import "dialoguefit/model"

type State string

const (
	Pending   State = "pending"
	Running   State = "running"
	Committed State = "committed"
	Failed    State = "failed"
)

type Machine struct {
	DialogueID string
	Current    State
}

func New(dialogueID string) *Machine {
	return &Machine{DialogueID: dialogueID, Current: Pending}
}

func (m *Machine) Transition(next State) error {
	allowed := false
	switch m.Current {
	case Pending:
		allowed = next == Running
	case Running:
		allowed = next == Committed || next == Failed
	case Failed:
		allowed = next == Running
	case Committed:
		allowed = next == Running
	}
	if !allowed {
		return model.StateError(m.DialogueID, "state", "illegal transition %s -> %s", m.Current, next)
	}
	m.Current = next
	return nil
}

func (m *Machine) Retry() error {
	if m.Current != Failed {
		return model.StateError(m.DialogueID, "state", "retry is only allowed from failed, current=%s", m.Current)
	}
	return m.Transition(Running)
}
