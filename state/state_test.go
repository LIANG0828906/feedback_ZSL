package state_test

import (
	"testing"

	"iconflow/errs"
	"iconflow/state"
)

func TestBlockLegalTransitionsAndFreeze(t *testing.T) {
	machine := state.NewBlockMachine("b")
	if err := machine.Transition(state.StageGradient, state.StatusCommitted); err == nil {
		t.Fatal("pending -> committed must be rejected")
	} else if err.Code != errs.EState {
		t.Fatalf("expected E_STATE, got %s", err.Code)
	}
	if machine.Stages[state.StageGradient] != state.StatusPending {
		t.Fatal("illegal transition changed state")
	}
	if err := machine.Transition(state.StageGradient, state.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(state.StageGradient, state.StatusFailed); err != nil {
		t.Fatal(err)
	}
	if err := machine.Retry(state.StageGradient); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(state.StageGradient, state.StatusCommitted); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(state.StageGradient, state.StatusRunning); err == nil {
		t.Fatal("committed stage must be frozen")
	}
}

func TestWindowTransitions(t *testing.T) {
	window := state.NewWindowMachine("p")
	if err := window.Transition(state.WindowCommitted); err == nil {
		t.Fatal("pending -> committed must fail")
	}
	if err := window.Transition(state.WindowSegmenting); err != nil {
		t.Fatal(err)
	}
	if err := window.Transition(state.WindowFailed); err != nil {
		t.Fatal(err)
	}
	if err := window.Transition(state.WindowSegmenting); err != nil {
		t.Fatal(err)
	}
	if err := window.Transition(state.WindowCommitted); err != nil {
		t.Fatal(err)
	}
	if err := window.Transition(state.WindowFailed); err == nil {
		t.Fatal("committed window must be frozen")
	}
}
