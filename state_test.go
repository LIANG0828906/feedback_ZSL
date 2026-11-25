package lipsync

import "testing"

func TestWindowStateMachine(t *testing.T) {
	window := NewWindow("dlg-test", 0, 0, 12)
	if window.State() != StatePending || window.Progress() != 0 {
		t.Fatalf("initial state = %s/%d", window.State(), window.Progress())
	}
	if err := window.Transition(StateCommitted); ErrorCode(err) != CodeState {
		t.Fatalf("illegal transition code = %s", ErrorCode(err))
	}
	if window.State() != StatePending || window.Progress() != 0 {
		t.Fatal("illegal transition changed state or progress")
	}
	if err := window.SetProgress(4); err != nil {
		t.Fatal(err)
	}
	for _, state := range []WindowState{StateScheduling, StateLipMapping, StateCommitted} {
		if err := window.Transition(state); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}
	if window.State() != StateCommitted || window.Progress() != 4 {
		t.Fatalf("final state = %s/%d", window.State(), window.Progress())
	}
	if len(window.History()) != 3 {
		t.Fatalf("history length = %d, want 3", len(window.History()))
	}
	if err := window.Transition(StateScheduling); ErrorCode(err) != CodeState {
		t.Fatalf("committed window accepted transition: %v", err)
	}
}

func TestWindowFailureAndProgressBounds(t *testing.T) {
	window := NewWindow("dlg-test", 1, 12, 24)
	if err := window.SetProgress(13); ErrorCode(err) != CodeState {
		t.Fatalf("out-of-range progress code = %s", ErrorCode(err))
	}
	if window.Progress() != 0 {
		t.Fatal("invalid progress mutated the window")
	}
	if err := window.Transition(StateScheduling); err != nil {
		t.Fatal(err)
	}
	if err := window.SetProgress(5); err != nil {
		t.Fatal(err)
	}
	stageErr := window.Fail("lip_mapping", 17, "injected")
	if ErrorCode(stageErr) != CodeStage || window.State() != StateFailed || window.Progress() != 5 {
		t.Fatalf("failure state = %s/%d err=%v", window.State(), window.Progress(), stageErr)
	}
	if err := window.Transition(StatePending); err != nil {
		t.Fatalf("failed -> pending should be a legal retry: %v", err)
	}
	if err := window.Transition(StateCommitted); ErrorCode(err) != CodeState {
		t.Fatalf("pending -> committed should be illegal: %v", err)
	}
}
