package statemachine

import (
	"testing"

	"vqa/internal/errs"
)

func TestLegalLifecycle(t *testing.T) {
	m := New(3)
	for _, next := range []State{Evaluating, Committed} {
		if err := m.Transition(next); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}
	if m.State != Committed {
		t.Fatalf("state = %s", m.State)
	}
}

func TestFailedRetryLifecycle(t *testing.T) {
	m := New(1)
	if err := m.Transition(Evaluating); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(Failed); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(Evaluating); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(Committed); err != nil {
		t.Fatal(err)
	}
}

func TestIllegalTransitionKeepsState(t *testing.T) {
	m := New(2)
	err := m.Transition(Committed) // pending -> committed is illegal
	if err == nil || errs.CodeOf(err) != errs.EState {
		t.Fatalf("want E_STATE, got %v", err)
	}
	if m.State != Pending {
		t.Fatalf("state mutated to %s", m.State)
	}
	// A committed block can never move again.
	_ = m.Transition(Evaluating)
	_ = m.Transition(Committed)
	err = m.Transition(Failed)
	if errs.CodeOf(err) != errs.EState {
		t.Fatalf("committed block moved: %v", err)
	}
	if m.State != Committed {
		t.Fatalf("committed state changed to %s", m.State)
	}
}
