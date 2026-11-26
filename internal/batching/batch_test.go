package batching_test

import (
	"testing"

	"wardrobekernel/internal/batching"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/samplegen"
	"wardrobekernel/internal/xerr"
)

func TestGeneratedBatchPlanBudgetsAndOffsets(t *testing.T) {
	project, err := samplegen.Generate(model.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := batching.Plan(project)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Batches) != 4 {
		t.Fatalf("expected 4 batches, got %d: %#v", len(plan.Batches), plan.CommitSequence)
	}
	for _, batch := range plan.Batches {
		if batch.VertexUsed > project.Constraints.MaxVertexBudget {
			t.Fatalf("batch %d vertex budget exceeded: %d > %d", batch.Number, batch.VertexUsed, project.Constraints.MaxVertexBudget)
		}
		if batch.IndexUsed > project.Constraints.MaxIndexBudget {
			t.Fatalf("batch %d index budget exceeded: %d > %d", batch.Number, batch.IndexUsed, project.Constraints.MaxIndexBudget)
		}
		if len(batch.Slots) > project.Constraints.MaxMaterialSlots {
			t.Fatalf("batch %d material slot budget exceeded", batch.Number)
		}

		partCount := 0
		for _, placement := range batch.Placements {
			partCount++
			if placement.VertexBase < 0 || placement.IndexBase < 0 {
				t.Fatalf("negative base in %#v", placement)
			}
			for _, sm := range placement.Submeshes {
				if sm.VertexStart >= sm.VertexEnd {
					t.Fatalf("invalid vertex interval %#v", sm)
				}
				if sm.IndexStart >= sm.IndexEnd || sm.IndexEnd-sm.IndexStart != sm.IndexCount {
					t.Fatalf("invalid index interval %#v", sm)
				}
				if sm.VertexEnd > batch.VertexUsed {
					t.Fatalf("placement exceeds batch vertex total: %#v used=%d", sm, batch.VertexUsed)
				}
				if sm.IndexEnd > batch.IndexUsed {
					t.Fatalf("placement exceeds batch index total: %#v used=%d", sm, batch.IndexUsed)
				}
			}
		}
		if partCount != len(batch.PartIDs) {
			t.Fatalf("batch %d placements/part ids mismatch", batch.Number)
		}
	}

	kinds := map[string]bool{}
	for _, d := range plan.Decisions {
		kinds[d.Kind] = true
		if d.BatchNumber < 0 || d.CandidateID == "" || d.Result == "" || len(d.Basis) == 0 {
			t.Fatalf("incomplete arbitration decision: %#v", d)
		}
	}
	for _, kind := range []string{"MATERIAL_SLOT_CONTENTION", "BUDGET_SQUEEZE"} {
		if !kinds[kind] {
			t.Fatalf("expected explainable %s decision, got %#v", kind, kinds)
		}
	}
}

func TestStrictTieReturnsEArbWithParties(t *testing.T) {
	project := samplegen.AmbiguousArbProject()
	_, err := batching.Plan(project)
	coded, ok := xerr.AsError(err)
	if !ok || coded.Code != xerr.EArb {
		t.Fatalf("expected E_ARB, got %#v", err)
	}
	if len(coded.Parties) != 2 || len(coded.BasisChain) < 3 {
		t.Fatalf("E_ARB must list parties and basis chain: %#v", coded)
	}
}
