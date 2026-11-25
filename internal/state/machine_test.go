package state_test

import (
	"testing"

	"skeletonretarget/internal/model"
	"skeletonretarget/internal/state"
)

func TestLegalTransitionsAndIllegalPreservation(t *testing.T) {
	machine := state.New(state.PendingLoad, 3)
	if err := machine.Transition(state.PendingRetarget, 4); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(state.Retargeting, 5); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(state.Committed, 8); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(state.PendingLoad, 9); !model.IsCode(err, model.CodeState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	snapshot := machine.Snapshot()
	if snapshot.State != state.Committed || snapshot.Progress != 8 {
		t.Fatalf("illegal transition mutated state: %+v", snapshot)
	}
}

func TestFailedRetryTransitions(t *testing.T) {
	machine := state.New(state.PendingRetarget, 0)
	if err := machine.Transition(state.Retargeting, 1); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(state.Failed, 1); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(state.PendingRetarget, 1); err != nil {
		t.Fatal(err)
	}
	if state.Legal(state.Failed, state.Committed) {
		t.Fatal("failed interval cannot commit directly")
	}
}
