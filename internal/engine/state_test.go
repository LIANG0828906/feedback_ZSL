package engine

import (
	"testing"

	"audioeval/internal/model"
)

func TestStateMachineRejectsIllegalTransitionsAndFreezesCommits(t *testing.T) {
	machine, err := NewStateMachine(2, 3)
	if err != nil {
		t.Fatal(err)
	}
	coord := model.BlockCoord{Frame: 0, Block: 0}
	if err := machine.Transition(coord, model.StageMetrics, model.StatusCommitted); err == nil || model.CodeOf(err) != model.CodeState {
		t.Fatalf("expected E_STATE for pending->committed, got %v", err)
	}
	if status := machine.Status(coord, model.StageMetrics); status != model.StatusPending {
		t.Fatalf("illegal transition changed state to %s", status)
	}
	if err := machine.MarkProcessing(coord); err != nil {
		t.Fatal(err)
	}
	result := model.BlockResult{Frame: 0, Block: 0, Quality: model.BlockQuality{PSNRUDb: 123}}
	if err := machine.CommitResult(coord, result); err != nil {
		t.Fatal(err)
	}
	if committed, total := machine.Progress(); committed != 1 || total != 6 {
		t.Fatalf("unexpected progress %d/%d", committed, total)
	}
	if err := machine.Transition(coord, model.StageMetrics, model.StatusProcessing); err == nil || model.CodeOf(err) != model.CodeState {
		t.Fatalf("expected E_STATE for committed->processing, got %v", err)
	}
	frozen, ok := machine.FrozenResult(coord)
	if !ok || frozen.Quality.PSNRUDb != 123 {
		t.Fatal("committed result was not frozen")
	}
	if updated := machine.UpdatePendingParamVersion("params-v2"); updated != 5 {
		t.Fatalf("parameter update must affect only 5 uncommitted blocks, got %d", updated)
	}
	if machine.ParamVersion(coord) != "" {
		t.Fatal("parameter update changed a committed block")
	}
	other := model.BlockCoord{Frame: 1, Block: 2}
	if machine.ParamVersion(other) != "params-v2" {
		t.Fatal("parameter update did not reach an uncommitted block")
	}
}
