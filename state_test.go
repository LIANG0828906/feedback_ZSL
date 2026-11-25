package textureconv

import "testing"

func TestStateMachineLegalAndIllegalTransitions(t *testing.T) {
	sm := NewStateMachine([]int{1, 2}, 2)
	if err := sm.Transition(1, 0, StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := sm.Transition(1, 0, StateCommitted); err != nil {
		t.Fatal(err)
	}
	err := sm.Transition(1, 0, StateProcessing)
	e := mustCode(t, err, CodeState)
	if e.Entry != 1 || e.Tile != 0 {
		t.Fatalf("bad state error context: %+v", e)
	}
	if got := sm.State(1, 0); got != StateCommitted {
		t.Fatalf("illegal transition changed state to %s", got)
	}
	err = sm.Transition(2, 1, StateFailed)
	mustCode(t, err, CodeState)
	if err := sm.Transition(2, 1, StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := sm.Transition(2, 1, StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := sm.Transition(2, 1, StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := sm.Transition(2, 1, StateCommitted); err != nil {
		t.Fatal(err)
	}
}

func TestStateMachineProgress(t *testing.T) {
	sm := NewStateMachine([]int{1, 2}, 2)
	for _, e := range []int{1, 2} {
		_ = sm.Transition(e, 0, StateProcessing)
		_ = sm.Transition(e, 0, StateCommitted)
	}
	p := sm.Progress()
	if p.CommittedTiles != 1 || p.TotalTiles != 2 {
		t.Fatalf("unexpected progress: %+v", p)
	}
}
