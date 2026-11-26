package statefx

import (
	"testing"

	"acousticfield/internal/acoustic"
)

func TestChunkStateLegalAndIllegalTransitions(t *testing.T) {
	m := NewChunk("C00")
	if m.State() != ChunkPending {
		t.Fatalf("initial state = %s", m.State())
	}
	if err := m.Transition(ChunkScreening); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(ChunkTransfer); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(ChunkCommitted); err != nil {
		t.Fatal(err)
	}
	before := m.State()
	err := m.Transition(ChunkScreening)
	if err == nil {
		t.Fatal("expected E_STATE")
	}
	ae, ok := acoustic.AsError(err)
	if !ok || ae.Code != acoustic.CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if m.State() != before {
		t.Fatalf("state changed on illegal transition: %s", m.State())
	}
}

func TestChunkFailureCanRetry(t *testing.T) {
	m := NewChunk("C01")
	_ = m.Transition(ChunkScreening)
	if err := m.Transition(ChunkFailed); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(ChunkScreening); err != nil {
		t.Fatalf("failed chunk should retry: %v", err)
	}
	if m.State() != ChunkScreening {
		t.Fatalf("state = %s", m.State())
	}
}

func TestRoundStateMachine(t *testing.T) {
	r := NewRound("round")
	if err := r.Transition(RoundRunning); err != nil {
		t.Fatal(err)
	}
	if err := r.Transition(RoundCommitted); err != nil {
		t.Fatal(err)
	}
	if err := r.Transition(RoundRunning); err == nil {
		t.Fatal("committed round must be terminal")
	}
}
