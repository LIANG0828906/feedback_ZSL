package state

import (
	"testing"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
)

func TestLegalTransitionsAndIllegalPreservesState(t *testing.T) {
	tile := model.TileCoord{TX: 2, TY: 3}
	m := NewMachine(tile)
	if err := m.Transition(StageForeground, StatusCommitted); err == nil {
		t.Fatal("pending -> committed must be illegal")
	} else if ee, ok := errs.As(err); !ok || ee.Code != errs.EState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if m.States[0] != StatusPending {
		t.Fatalf("illegal transition changed state to %s", m.States[0])
	}
	for _, to := range []Status{StatusProcessing, StatusCommitted} {
		if err := m.Transition(StageForeground, to); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Transition(StageForeground, StatusProcessing); err == nil {
		t.Fatal("committed stage must be frozen")
	}
}

func TestMatrixProgressAndSequence(t *testing.T) {
	tiles := []model.TileCoord{{TX: 0, TY: 0}, {TX: 1, TY: 0}}
	m := NewMatrix(tiles)
	if m.TotalStages() != 6 || m.CommittedStages() != 0 {
		t.Fatal("bad initial progress")
	}
	_ = m.Transition(tiles[0], StageForeground, StatusProcessing)
	_ = m.Transition(tiles[0], StageForeground, StatusCommitted)
	if m.CommittedStages() != 1 || len(m.Sequence()) != 1 {
		t.Fatal("commit not recorded")
	}
}
