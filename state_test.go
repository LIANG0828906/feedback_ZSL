package audiofp_test

import (
	"errors"
	"testing"

	"audiofp"
)

func TestWindowStateMachineLegalTransitions(t *testing.T) {
	params := audiofp.DefaultParams()
	machine := audiofp.NewFrameWindowMachine(100000, params)
	if committed, total := machine.Progress(); committed != 0 || total <= 0 {
		t.Fatalf("bad initial progress %d/%d", committed, total)
	}

	if err := machine.Transition(0, audiofp.StateCommitted); !audiofp.HasCode(err, audiofp.CodeState) {
		t.Fatalf("pending->committed must be E_STATE, got %v", err)
	}
	if w, _ := machine.Window(0); w.State != audiofp.StatePending {
		t.Fatalf("illegal transition changed state to %s", w.State)
	}

	if err := machine.Start(0); err != nil {
		t.Fatal(err)
	}
	if err := machine.Start(0); !audiofp.HasCode(err, audiofp.CodeState) {
		t.Fatalf("extracting->extracting must be illegal, got %v", err)
	}
	if fp, err := audiofp.ExtractFrameAt(make([]float64, params.FrameLength), 0, 16000, params); err != nil {
		t.Fatal(err)
	} else if err := machine.Commit(0, fp, params); err != nil {
		t.Fatal(err)
	}
	if err := machine.Start(0); !audiofp.HasCode(err, audiofp.CodeState) {
		t.Fatalf("committed window must be terminal/frozen, got %v", err)
	}
	if w, _ := machine.Window(0); w.State != audiofp.StateCommitted || w.Fingerprint == nil {
		t.Fatalf("committed window not frozen: %#v", w)
	}

	if err := machine.Start(1); err != nil {
		t.Fatal(err)
	}
	if err := machine.Fail(1, errors.New("simulated extraction failure")); err != nil {
		t.Fatal(err)
	}
	if err := machine.Transition(1, audiofp.StateCommitted); !audiofp.HasCode(err, audiofp.CodeState) {
		t.Fatalf("failed->committed must be illegal, got %v", err)
	}
	if err := machine.Start(1); err != nil {
		t.Fatalf("failed->extracting retry should be legal: %v", err)
	}
}

func TestCommittedParameterSnapshotFrozen(t *testing.T) {
	params := audiofp.DefaultParams()
	machine := audiofp.NewFrameWindowMachine(100000, params)
	if err := machine.Start(0); err != nil {
		t.Fatal(err)
	}
	fp, err := audiofp.ExtractFrameAt(make([]float64, params.FrameLength), 0, 16000, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Commit(0, fp, params); err != nil {
		t.Fatal(err)
	}

	newParams := params
	newParams.BandCount = 48
	changed := machine.UpdateUncommittedParameters(newParams)
	if changed == 0 {
		t.Fatalf("expected pending windows to be updated")
	}
	w0, _ := machine.Window(0)
	if w0.ParamSnapshot.BandCount != params.BandCount || w0.ParamVersion != params.Version() {
		t.Fatalf("committed snapshot changed: %#v", w0.ParamSnapshot)
	}
	w1, _ := machine.Window(1)
	if w1.ParamSnapshot.BandCount != 48 {
		t.Fatalf("uncommitted parameter not updated: %#v", w1.ParamSnapshot)
	}
}
