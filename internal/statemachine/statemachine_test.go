package statemachine

import (
	"testing"

	"spatialir/internal/model"
)

func TestTransitionsAndFreeze(t *testing.T) {
	m := New(3)
	if err := m.Transition(0, model.StatusCommitted); err == nil {
		t.Fatal("pending->committed must be illegal")
	}
	if m.Status(0) != model.StatusPending {
		t.Fatal("illegal transition must keep original state")
	}
	if err := m.Transition(0, model.StatusEstimated); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(0, model.StatusCommitted); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(0, model.StatusPending); err == nil {
		t.Fatal("committed frame must be frozen")
	}
	if err := m.Transition(1, model.StatusFailed); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(1, model.StatusPending); err != nil {
		t.Fatal("failed frame must be retryable")
	}
	committed, total := m.Progress()
	if committed != 1 || total != 3 {
		t.Fatalf("progress=%d/%d", committed, total)
	}
}
