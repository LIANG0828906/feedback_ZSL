package segment_test

import (
	"testing"

	"captionx/internal/errs"
	"captionx/internal/segment"
)

func TestLegalTransitions(t *testing.T) {
	m := segment.NewMachine(2)
	steps := []segment.State{segment.Processing, segment.Committed}
	for _, s := range steps {
		if err := m.Transition(0, s); err != nil {
			t.Fatalf("transition to %s: %v", s, err)
		}
	}
	if m.State(0) != segment.Committed {
		t.Fatalf("state = %s", m.State(0))
	}
	c, total := m.Progress()
	if c != 1 || total != 2 {
		t.Fatalf("progress = %d/%d", c, total)
	}
}

func TestIllegalTransitionKeepsState(t *testing.T) {
	m := segment.NewMachine(1)
	if err := m.Transition(0, segment.Committed); errs.Code(err) != errs.EState {
		t.Fatalf("code = %s, want E_STATE", errs.Code(err))
	}
	if m.State(0) != segment.Pending {
		t.Fatalf("state changed after illegal transition: %s", m.State(0))
	}
	// Committed is frozen.
	if err := m.Transition(0, segment.Processing); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(0, segment.Committed); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(0, segment.Processing); errs.Code(err) != errs.EState {
		t.Fatalf("code = %s, want E_STATE", errs.Code(err))
	}
	if m.State(0) != segment.Committed {
		t.Fatalf("committed chunk not frozen")
	}
}

func TestFailedCanRetry(t *testing.T) {
	m := segment.NewMachine(1)
	_ = m.Transition(0, segment.Processing)
	if err := m.Transition(0, segment.Failed); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(0, segment.Processing); err != nil {
		t.Fatalf("retry from failed: %v", err)
	}
	if err := m.Transition(0, segment.Pending); errs.Code(err) != errs.EState {
		t.Fatalf("code = %s, want E_STATE", errs.Code(err))
	}
}

func TestChunkMapping(t *testing.T) {
	if got := segment.ChunkCount(5, 2); got != 3 {
		t.Fatalf("ChunkCount = %d", got)
	}
	if got := segment.ChunkOf(0, 100); got != 0 {
		t.Fatalf("ChunkOf = %d", got)
	}
	if got := segment.ChunkOf(250, 100); got != 2 {
		t.Fatalf("ChunkOf = %d", got)
	}
}
