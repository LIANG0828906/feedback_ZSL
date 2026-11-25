package packetstate

import (
	"testing"

	"github.com/liang/hapticfeedback/internal/haptic/errs"
)

func TestStateMachineLegalAndIllegalTransitions(t *testing.T) {
	m := &Machine{Seq: 7, Channel: "ch_left", State: Pending}
	if err := m.Transition(Committed); err == nil || errs.CodeOf(err) != errs.EState {
		t.Fatalf("pending->committed must fail E_STATE, got %v", err)
	}
	if m.State != Pending {
		t.Fatalf("failed transition must preserve state, got %s", m.State)
	}
	for _, to := range []State{Quantizing, Split, Committed} {
		if err := m.Transition(to); err != nil {
			t.Fatalf("legal transition to %s failed: %v", to, err)
		}
	}
	if err := m.Transition(Quantizing); err == nil || errs.CodeOf(err) != errs.EState {
		t.Fatalf("committed packet must be frozen, got err=%v", err)
	}
	if m.State != Committed {
		t.Fatalf("committed state changed after illegal transition: %s", m.State)
	}

	failed := &Machine{State: Quantizing}
	stageErr := errs.New(errs.EStage, "boom")
	if err := failed.Transition(Failed); err != nil {
		t.Fatal(err)
	}
	failed.LastError = stageErr
	if err := failed.Transition(Quantizing); err != nil {
		t.Fatalf("failed packet must support retry: %v", err)
	}
	if failed.State != Quantizing || failed.Attempts != 1 {
		t.Fatalf("unexpected retry state/attempts: %s %d", failed.State, failed.Attempts)
	}
}

func TestTrackerProgress(t *testing.T) {
	tr := NewTracker()
	tr.Add(0, "a")
	tr.Add(1, "b")
	if c, total := tr.Progress(); c != 0 || total != 2 {
		t.Fatalf("progress=%d/%d", c, total)
	}
	if err := tr.Transition(0, Quantizing, nil); err != nil {
		t.Fatal(err)
	}
	if err := tr.Transition(0, Split, nil); err != nil {
		t.Fatal(err)
	}
	if err := tr.Transition(0, Committed, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := tr.Progress(); c != 1 {
		t.Fatalf("committed progress=%d", c)
	}
}
