package engine_test

import (
	"testing"

	"battlekernel/internal/engine"
	"battlekernel/internal/errs"
	"battlekernel/internal/model"
	"battlekernel/internal/sample"
)

func TestEngineMatchesGeneratorTruth(t *testing.T) {
	suite, err := sample.GenerateSuite(sample.DefaultSeed, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range suite.Samples {
		results, _, err := engine.SimulateAll(s.Project)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.VerifyTruth(s.Project, results); err != nil {
			t.Fatalf("%s: %v", s.Project.ID, err)
		}
	}
}

func TestOrderMismatchReturnsEOrder(t *testing.T) {
	p := mustProject(t)
	results, _, err := engine.SimulateAll(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Truth.Rounds[0].Order[0].UnitID = "uX"
	err = engine.VerifyTruth(p, results)
	if !errs.IsCode(err, errs.EOrder) {
		t.Fatalf("expected E_ORDER, got %v", err)
	}
}

func TestStateMismatchReturnsEState(t *testing.T) {
	p := mustProject(t)
	results, _, err := engine.SimulateAll(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Truth.Rounds[0].States[0].Resource += 999
	err = engine.VerifyTruth(p, results)
	if !errs.IsCode(err, errs.EState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
}

func TestCooldownOutOfBoundsReturnsECool(t *testing.T) {
	p := mustProject(t)
	states := engine.InitialStateMap(p)
	state := states["u1"]
	state.CooldownRemaining = 101
	states["u1"] = state
	_, _, err := engine.SettleRound(p, p.Rounds[0], states)
	if !errs.IsCode(err, errs.ECool) {
		t.Fatalf("expected E_COOL, got %v", err)
	}
}

func TestMissingConditionalFlagReturnsEFlag(t *testing.T) {
	p := mustProject(t)
	for i := range p.Rounds[0].Actions {
		if p.Rounds[0].Actions[i].SkillID == "s3-rite" {
			p.Rounds[0].Actions[i].Flag = nil
		}
	}
	_, _, err := engine.AdjudicateQueue(p, p.Rounds[0], engine.InitialStateMap(p))
	if !errs.IsCode(err, errs.EFlag) {
		t.Fatalf("expected E_FLAG, got %v", err)
	}
}

func mustProject(t *testing.T) *model.Project {
	t.Helper()
	suite, err := sample.GenerateSuite(sample.DefaultSeed, 1)
	if err != nil {
		t.Fatal(err)
	}
	return suite.Samples[0].Project
}
