package segment

import (
	"testing"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/model"
)

func TestStateMachineRejectsIllegalTransitionWithoutMutation(t *testing.T) {
	plan, err := Plan(30, 100000, 1)
	if err != nil {
		t.Fatal(err)
	}
	m := New(plan)
	err = m.Transition(0, StateCommitted)
	if e := apperr.As(err); e == nil || e.Code != apperr.State {
		t.Fatalf("pending->committed error = %#v", e)
	}
	if got := m.State(0); got != StatePending {
		t.Fatalf("state mutated to %s", got)
	}
	if e := apperr.As(m.Transition(0, StateCommitted)); e == nil || e.Code != apperr.State {
		t.Fatalf("expected E_STATE, got %#v", e)
	}
	if err := m.Begin(0); err != nil {
		t.Fatal(err)
	}
	result := model.SegmentResult{Index: 0, StartFrame: 0, EndFrame: 9, Frames: make([]model.FrameResult, 10)}
	if err := m.Commit(0, result); err != nil {
		t.Fatal(err)
	}
	committed, total := m.Progress()
	if committed != 1 || total != 3 {
		t.Fatalf("progress = %d/%d", committed, total)
	}
}

func TestCommittedResultIsFrozen(t *testing.T) {
	plan, _ := Plan(10, 100000, 1)
	m := New(plan)
	_ = m.Begin(0)
	frames := make([]model.FrameResult, 10)
	frames[0].Summary = "original"
	result := model.SegmentResult{Index: 0, StartFrame: 0, EndFrame: 9, Frames: frames}
	if err := m.Commit(0, result); err != nil {
		t.Fatal(err)
	}
	result.Frames[0].Summary = "mutated"
	got, _ := m.Segment(0)
	if got.Result.Frames[0].Summary != "original" {
		t.Fatal("committed snapshot was mutated")
	}
	if e := apperr.As(m.Transition(0, StateVerifying)); e == nil || e.Code != apperr.State {
		t.Fatal("committed segment accepted a transition")
	}
}

func TestPlanFixedDuration(t *testing.T) {
	plan, err := Plan(36, 100000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 4 || plan[3].StartFrame != 30 || plan[3].EndFrame != 35 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}
