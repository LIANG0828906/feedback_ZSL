package checkpoint

import (
	"errors"
	"testing"

	"mocapworkbench/internal/model"
)

func TestStateMachineTransitions(t *testing.T) {
	machine := NewStateMachine(2)
	if err := machine.Transition(0, StateSolving); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(0, StateCommitted); err != nil {
		t.Fatal(err)
	}
	err := machine.Transition(0, StateSolving)
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Code != model.ErrState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	state, _ := machine.State(0)
	if state != StateCommitted {
		t.Fatalf("illegal transition changed state to %s", state)
	}
	if err := machine.Transition(1, StateSolving); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(1, StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(1, StatePending); err != nil {
		t.Fatal(err)
	}
}
