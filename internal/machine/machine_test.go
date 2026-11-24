package machine_test

import (
	"testing"

	"audioforge/internal/errs"
	"audioforge/internal/machine"
)

func TestIllegalTransitionKeepsOriginalState(t *testing.T) {
	packet := machine.New(2, 9)
	if err := packet.Transition(machine.Sliced); !errs.IsCode(err, errs.EState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if packet.State() != machine.PendingQuantization {
		t.Fatalf("state changed to %s", packet.State())
	}
	if err := packet.Transition(machine.Quantizing); err != nil {
		t.Fatal(err)
	}
	if err := packet.Transition(machine.Committed); !errs.IsCode(err, errs.EState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if packet.State() != machine.Quantizing {
		t.Fatalf("state changed to %s", packet.State())
	}
	if err := packet.Transition(machine.Sliced); err != nil {
		t.Fatal(err)
	}
	if err := packet.Transition(machine.Committed); err != nil {
		t.Fatal(err)
	}
	if err := packet.Transition(machine.Failed); !errs.IsCode(err, errs.EState) {
		t.Fatalf("committed should be terminal, got %v", err)
	}
}
