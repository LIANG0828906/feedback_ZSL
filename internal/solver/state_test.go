package solver_test

import (
	"testing"

	"levelacoustic/internal/model"
	"levelacoustic/internal/solver"
)

func TestStateMachineRejectsIllegalTransitions(t *testing.T) {
	m := solver.NewStateMachine([]string{"R1"})
	for _, state := range []model.RoomState{
		model.StateAnalyzingConnectivity,
		model.StateSolvingParameters,
		model.StateCommitted,
	} {
		if err := m.Transition("R1", state); err != nil {
			t.Fatalf("legal transition to %s: %v", state, err)
		}
	}
	err := m.Transition("R1", model.StateAnalyzingConnectivity)
	if err == nil {
		t.Fatal("expected E_STATE for committed -> analyzing")
	}
	me, ok := model.AsError(err)
	if !ok || me.Code != model.ErrState {
		t.Fatalf("want E_STATE, got %v", err)
	}
	if got := m.State("R1"); got != model.StateCommitted {
		t.Fatalf("illegal transition changed state to %s", got)
	}
	if err := m.InvalidateForRevision("R1", "dependency_changed"); err != nil {
		t.Fatal(err)
	}
	if got := m.State("R1"); got != model.StatePending {
		t.Fatalf("invalidation state = %s", got)
	}
}

func TestStateMachineFailureAndRetry(t *testing.T) {
	m := solver.NewStateMachine([]string{"R1"})
	if err := m.Transition("R1", model.StateAnalyzingConnectivity); err != nil {
		t.Fatal(err)
	}
	m.Fail("R1", "test_failure")
	if got := m.State("R1"); got != model.StateFailed {
		t.Fatalf("state = %s", got)
	}
	if err := m.Transition("R1", model.StatePending); err != nil {
		t.Fatal(err)
	}
	if got := m.State("R1"); got != model.StatePending {
		t.Fatalf("state = %s", got)
	}
}
