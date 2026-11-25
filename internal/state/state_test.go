package state

import (
	"errors"
	"testing"

	aerr "audiobus/errs"
)

func TestLegalAndIllegalTransitions(t *testing.T) {
	m := New("packet", 2, 1, 3)
	if err := m.Transition(Quantizing); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(Sliced); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(Committed); err != nil {
		t.Fatal(err)
	}
	err := m.Transition(Quantizing)
	var ae *aerr.Error
	if !errors.As(err, &ae) || ae.Code != aerr.EState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if m.Current != Committed {
		t.Fatalf("state changed after illegal transition: %s", m.Current)
	}

	failed := New("window", 0, 0, 0)
	_ = failed.Transition(Quantizing)
	if err := failed.Transition(Failed); err != nil {
		t.Fatal(err)
	}
	if err := failed.Transition(Pending); err != nil {
		t.Fatalf("retry should be allowed: %v", err)
	}
}
