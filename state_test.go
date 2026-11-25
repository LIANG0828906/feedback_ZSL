package dialogue

import (
	"errors"
	"testing"
)

func TestClipStateMachineLegalAndIllegalTransitions(t *testing.T) {
	machine := NewMachine([]string{"c1"})
	before := machine.Snapshot()
	if before.CommittedClips != 0 || before.ProgressPermille != 0 {
		t.Fatalf("unexpected initial progress: %+v", before)
	}

	err := machine.Transition("c1", StateCommitted)
	var be *Error
	if !errors.As(err, &be) || be.Code != CodeState || be.FromState != StatePending || be.ToState != StateCommitted {
		t.Fatalf("expected E_STATE for pending->committed, got %#v", err)
	}
	afterIllegal := machine.Snapshot()
	if afterIllegal.States["c1"] != StatePending || afterIllegal.ProgressPermille != before.ProgressPermille {
		t.Fatalf("illegal transition changed state/progress: before=%+v after=%+v", before, afterIllegal)
	}

	for _, to := range []State{StateExtracting, StateCalibrating, StateCommitted} {
		if err := machine.Transition("c1", to); err != nil {
			t.Fatalf("legal transition to %s failed: %v", to, err)
		}
	}
	committed := machine.Snapshot()
	if committed.CommittedClips != 1 || committed.ProgressPermille != 1000 {
		t.Fatalf("commit progress wrong: %+v", committed)
	}
	if err := machine.Transition("c1", StateExtracting); err == nil {
		t.Fatal("committed clip must be terminal")
	}
}

func TestFrozenCommittedClipRejectsGainEdit(t *testing.T) {
	p, _, _ := sampleProject(t)
	engine := NewEngine(p)
	if _, pErr := engine.BuildAll(); pErr != nil {
		t.Fatal(pErr)
	}
	if pErr := engine.FreezeClip("clip-01"); pErr != nil {
		t.Fatal(pErr)
	}
	_, err := engine.ApplyClipGainEdit("clip-01", 10400)
	var be *Error
	if !errors.As(err, &be) || be.Code != CodeState {
		t.Fatalf("expected E_STATE when editing frozen clip, got %#v", err)
	}
}
