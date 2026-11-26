package checkpoint

import (
	"testing"

	"lip-pipeline-prototype/internal/domain"
)

func TestIllegalStateTransitionPreservesStateAndProgress(t *testing.T) {
	m := NewMachine("shard-1", 2)
	m.Progress = 37
	err := m.Transition(StateCommitted, "skip stages")
	de, ok := domain.AsError(err)
	if !ok || de.Code != domain.CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if m.State != StatePending || m.Progress != 37 {
		t.Fatalf("machine changed after illegal transition: %+v", m)
	}
}

func TestLegalStatePath(t *testing.T) {
	m := NewMachine("shard-1", 0)
	for _, next := range []State{StateQuantizing, StateMerging, StateDelivering, StateCommitted} {
		if err := m.Transition(next, ""); err != nil {
			t.Fatal(err)
		}
	}
	if m.Progress != 100 {
		t.Fatalf("progress=%d", m.Progress)
	}
}
