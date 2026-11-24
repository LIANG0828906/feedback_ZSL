package state

import (
	"testing"

	"texlab/internal/apperr"
	"texlab/internal/model"
)

func TestStateMachineLegalAndIllegalTransitions(t *testing.T) {
	m := NewMatrix(1, 1, 1, 1)
	e := model.Entry{Index: 1, ID: "e1"}
	if err := m.Transition(1, 0, Processing, 1, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(1, 0, Committed, 1, "ok", &e); err != nil {
		t.Fatal(err)
	}
	before := m.Get(1, 0)
	if err := m.Transition(1, 0, Processing, 1, "", nil); !apperr.IsCode(err, apperr.EState) {
		t.Fatalf("want E_STATE, got %v", err)
	}
	after := m.Get(1, 0)
	if after.State != before.State || after.SnapshotHash != before.SnapshotHash {
		t.Fatalf("illegal transition mutated state: %+v -> %+v", before, after)
	}
}

func TestStateMachineRejectsVersionMix(t *testing.T) {
	m := NewMatrix(1, 1, 2, 2)
	e := model.Entry{Index: 1, ID: "e1"}
	_ = m.Transition(1, 0, Processing, 2, "", nil)
	_ = m.Transition(1, 0, Committed, 2, "ok", &e)
	err := m.Transition(1, 0, Processing, 3, "", nil)
	if !apperr.IsCode(err, apperr.EVersion) {
		t.Fatalf("want E_VERSION, got %v", err)
	}
	ae := apperr.As(err)
	if ae.Segment != 0 || ae.Version != 3 || ae.Other != 2 {
		t.Fatalf("version context = %+v", ae)
	}
}
