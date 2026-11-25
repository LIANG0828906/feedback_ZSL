package core

import "testing"

func TestStateMachineRejectsIllegalTransitionWithoutMutation(t *testing.T) {
	m := NewMachine(StatePending, 1, &BlockRef{Index: 0}, "block")
	err := m.Transition(StateCommitted)
	if err == nil || CodeOf(err) != CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if got := m.State(); got != StatePending {
		t.Fatalf("state mutated on failure: %s", got)
	}
	if err := m.Transition(StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StatePending); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StateCommitted); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(StateProcessing); err == nil {
		t.Fatal("committed -> processing must be rejected")
	}
}
