package colortrace_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	ct "colortrace"
	"colortrace/sample"
)

func TestGeneratedTruthOutputsAttributionAndBounds(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !ct.VerifyTruthFingerprint(project.Truth) {
		t.Fatal("truth fingerprint does not verify")
	}
	result, err := ct.ExecuteSequential(project)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := ct.VerifyTruthOutputs(project, result.Samples); mismatch != nil {
		t.Fatalf("truth mismatch: %s", ct.DescribeMismatch(mismatch))
	}
	if result.Band == nil {
		t.Fatal("expected generated band")
	}
	if result.Band.Stage != 1 || result.Band.Plane != "Y" || result.Band.Coord != (ct.Coord{X: 4, Y: 1}) {
		t.Fatalf("unexpected first band: %+v", result.Band)
	}
	if result.Band.ChunkID != "c01x00" || result.Band.DominantOperator != "bit_round" || result.Band.Ambiguous {
		t.Fatalf("band attribution mismatch: %+v", result.Band)
	}
	if _, err := ct.VerifyTruthBounds(project, result.Samples, project.Truth); err != nil {
		t.Fatalf("bounds failed: %v", err)
	}

	sawRemainder := false
	sawBitDelta := false
	for _, stage := range result.Samples {
		for _, plane := range stage.Planes {
			for _, ledger := range plane.Ledgers {
				if ledger.Breakdown.Sum() != ledger.TotalDeviation {
					t.Fatalf("breakdown sum mismatch at %s %v", plane.Name, ledger.Coord)
				}
				if ledger.Remainder != 0 {
					sawRemainder = true
				}
				if ledger.Breakdown.BitRound.Num != 0 {
					sawBitDelta = true
				}
			}
		}
	}
	if !sawRemainder || !sawBitDelta {
		t.Fatalf("expected truncation evidence remainder=%v bit=%v", sawRemainder, sawBitDelta)
	}
}

func TestSchemaMissingFieldPath(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	stages := raw["stages"].([]any)
	stageOne := stages[1].(map[string]any)
	delete(stageOne, "bits")
	raw["stages"] = stages
	edited, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ct.LoadProject(edited)
	var ce *ct.Error
	if !errors.As(err, &ce) || ce.Code != ct.ESchema {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	if ce.Path != "$.stages[1].bits" {
		t.Fatalf("unexpected path %q", ce.Path)
	}
}

func TestParameterBitsOutOfRange(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	project.Stages[1].Bits = 2
	_, err = ct.ProcessChunk(project, 1, nil, nil, ct.ChunkCoord{X: 0, Y: 0})
	var ce *ct.Error
	if !errors.As(err, &ce) || ce.Code != ct.EPara {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	if ce.Stage != 1 || ce.LegalRange[0] != ct.MinQuantBits {
		t.Fatalf("bad parameter error detail: %+v", ce)
	}
}

func TestAmbiguousAttributionListsShares(t *testing.T) {
	breakdown := ct.OperatorBreakdown{
		ResampleNorm: ct.I64(3),
		AccumTrunc:   ct.I64(3),
		BitRound:     ct.I64(3),
		Chroma:       ct.I64(3),
	}
	report, err := ct.ClassifyAttribution(1, "Cb", "c00x00", ct.Coord{X: 1, Y: 1}, 9000, 5000, breakdown, 0.6)
	if report == nil {
		t.Fatal("expected report alongside ambiguity error")
	}
	var ce *ct.Error
	if !errors.As(err, &ce) || ce.Code != ct.EAmbig {
		t.Fatalf("expected E_AMBIG, got %v", err)
	}
	if len(ce.Shares) != 4 {
		t.Fatalf("expected four competing shares, got %d", len(ce.Shares))
	}
}

func TestBoundViolationCode(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	result, err := ct.ExecuteSequential(project)
	if err != nil {
		t.Fatal(err)
	}
	tampered := *project.Truth
	points := append([]ct.TruthPoint{}, project.Truth.Points...)
	stages := append([]ct.TruthStageSample{}, points[0].Stages...)
	stages[0].ExpectedCumulative = ct.I64(1_000_000)
	points[0].Stages = stages
	tampered.Points = points
	_, err = ct.VerifyTruthBounds(project, result.Samples, &tampered)
	var ce *ct.Error
	if !errors.As(err, &ce) || ce.Code != ct.EBound || ce.Stage != 0 || ce.Coord == nil {
		t.Fatalf("expected E_BOUND with location, got %v", err)
	}
}

func TestIllegalStateTransitionPreservesState(t *testing.T) {
	machine := ct.NewChunkStateMachine()
	_, err := machine.Transition(0, "c00x00", ct.StateCommitted)
	var ce *ct.Error
	if !errors.As(err, &ce) || ce.Code != ct.EState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if state := machine.State(0, "c00x00"); state != ct.StatePending {
		t.Fatalf("state mutated to %s", state)
	}
}

func TestDecodeTruncationCode(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	data, err := ct.EncodePlane(project.Planes[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ct.DecodePlane(data[:len(data)-8]); err == nil {
		t.Fatal("expected truncation error")
	} else {
		var ce *ct.Error
		if !errors.As(err, &ce) || ce.Code != ct.EDecode {
			t.Fatalf("expected E_DECODE, got %v", err)
		}
	}
}

func TestOrderInversionCode(t *testing.T) {
	now := time.Now()
	err := ct.StageTiming{Stage: 1, StartedAt: now, FinishedAt: now.Add(-time.Second)}.Validate()
	var ce *ct.Error
	if !errors.As(err, &ce) || ce.Code != ct.EOrder || ce.Stage != 1 {
		t.Fatalf("expected E_ORDER, got %v", err)
	}
}

func TestDependencyGraphPreciseInvalidation(t *testing.T) {
	project, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := ct.BuildDependencyGraph(project)
	if err != nil {
		t.Fatal(err)
	}
	edited := project.Stages[1]
	kernel := *edited.ChromaKernel
	kernel.NormScale = 128
	edited.ChromaKernel = &kernel
	plan, err := graph.PlanParameterEdit(project, edited)
	if err != nil {
		t.Fatal(err)
	}
	invalid := map[string]bool{}
	for _, event := range plan.Invalidations {
		invalid[event.NodeID] = true
	}
	if !invalid[ct.StagePlaneNodeID(1, "Cb")] || !invalid[ct.JudgmentNodeID(1, "Cr")] {
		t.Fatalf("chroma nodes not invalidated: %+v", plan.Invalidations)
	}
	if invalid[ct.StagePlaneNodeID(1, "Y")] || invalid[ct.StagePlaneNodeID(0, "Y")] {
		t.Fatalf("unexpected luma invalidation: %+v", plan.Invalidations)
	}

	reordered := []ct.StageConfig{project.Stages[1], project.Stages[0]}
	reorderPlan, err := graph.PlanReorder(project, reordered)
	if err != nil {
		t.Fatal(err)
	}
	if len(reorderPlan.Invalidations) == 0 {
		t.Fatal("expected reorder invalidations")
	}
}
