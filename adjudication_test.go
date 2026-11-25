package chaosdiag_test

import (
	"testing"

	"chaosdiag/engine"
	"chaosdiag/model"
)

func TestAdjudicationUsesPriorityThenDeclarationOrder(t *testing.T) {
	candidates := []model.Instruction{
		{ID: "later-same-priority", Kind: model.KindDelay, Priority: 10, Sequence: 2},
		{ID: "earlier-same-priority", Kind: model.KindDelay, Priority: 10, Sequence: 1},
		{ID: "higher-priority", Kind: model.KindDelay, Priority: 20, Sequence: 0},
	}
	winners := engine.Adjudicate(candidates)
	if winners[model.KindDelay].ID != "higher-priority" {
		t.Fatalf("priority winner=%s", winners[model.KindDelay].ID)
	}

	samePriority := []model.Instruction{
		{ID: "later", Kind: model.KindTimeout, Priority: 7, Sequence: 8},
		{ID: "earlier", Kind: model.KindTimeout, Priority: 7, Sequence: 3},
	}
	winners = engine.Adjudicate(samePriority)
	if winners[model.KindTimeout].ID != "earlier" {
		t.Fatalf("sequence winner=%s", winners[model.KindTimeout].ID)
	}
}
