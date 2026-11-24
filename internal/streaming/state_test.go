package streaming

import (
	"testing"

	"occlusionlab/internal/model"
)

func TestWindowTransitionsAndIllegalStatePreservesState(t *testing.T) {
	window := NewWindow(3)
	if window.State != StatePending {
		t.Fatalf("initial state = %s", window.State)
	}
	if err := window.Transition(StateCommitted); err == nil ||
		err.Code != model.CodeState || err.Window != 3 {
		t.Fatalf("expected E_STATE window 3, got %+v", err)
	}
	if window.State != StatePending {
		t.Fatalf("illegal transition changed state to %s", window.State)
	}
	if err := window.Transition(StateCulling); err != nil {
		t.Fatal(err)
	}
	if err := window.Transition(StateLoading); err != nil {
		t.Fatal(err)
	}
	frame := &WindowFrame{Window: 3, Visible: []int{1, 2}}
	if err := window.Freeze(frame); err != nil {
		t.Fatal(err)
	}
	if window.State != StateCommitted || window.Frame == nil {
		t.Fatal("window did not freeze")
	}
	if err := window.Transition(StatePending); err == nil || err.Code != model.CodeState {
		t.Fatal("committed window accepted an illegal transition")
	}
}

func TestFreezeRequiresLoading(t *testing.T) {
	window := NewWindow(0)
	if err := window.Freeze(&WindowFrame{}); err == nil || err.Code != model.CodeState {
		t.Fatalf("expected E_STATE, got %+v", err)
	}
	if window.State != StatePending {
		t.Fatal("freeze failure mutated state")
	}
}

func TestFailedWindowCanRetry(t *testing.T) {
	window := NewWindow(0)
	if err := window.Transition(StateCulling); err != nil {
		t.Fatal(err)
	}
	if err := window.Transition(StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := window.Transition(StatePending); err != nil {
		t.Fatal(err)
	}
	if window.State != StatePending {
		t.Fatal("failed window did not retry")
	}
}
