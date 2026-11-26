package state_test

import (
	"testing"

	"dialogueaudio/audioerr"
	"dialogueaudio/state"
)

func TestIllegalTransitionDoesNotChangeStateOrProgress(t *testing.T) {
	manager, err := state.New([]string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.Transition("c1", state.Calibrating)
	if !audioerr.IsCode(err, audioerr.CodeState) {
		t.Fatalf("want E_STATE, got %v", err)
	}
	if status, _ := manager.StatusOf("c1"); status != state.Pending {
		t.Fatalf("status changed to %s", status)
	}
	if progress := manager.Progress(); progress.Committed != 0 || progress.Total != 2 {
		t.Fatalf("progress changed: %+v", progress)
	}
}

func TestCommitFreezesClipAndProgress(t *testing.T) {
	manager, err := state.New([]string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []state.Status{state.Measuring, state.Calibrating, state.Committed} {
		if err := manager.Transition("c1", status); err != nil {
			t.Fatal(err)
		}
	}
	if !manager.Frozen("c1") || manager.EditAllowed("c1") {
		t.Fatal("committed clip was not frozen")
	}
	if progress := manager.Progress(); progress.Committed != 1 || progress.Total != 2 {
		t.Fatalf("bad progress: %+v", progress)
	}
	if err := manager.Transition("c1", state.Measuring); !audioerr.IsCode(err, audioerr.CodeState) {
		t.Fatalf("want E_STATE from committed, got %v", err)
	}
	if progress := manager.Progress(); progress.Committed != 1 {
		t.Fatalf("progress changed after illegal transition: %+v", progress)
	}
}
