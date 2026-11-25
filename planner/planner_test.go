package planner_test

import (
	"math/rand"
	"reflect"
	"testing"

	"wardrobe/assembly"
	"wardrobe/diagnostics"
	"wardrobe/domain"
	"wardrobe/generator"
	"wardrobe/planner"
	"wardrobe/schema"
)

func TestGeneratedTruthMatchesAssemblyAndPlanner(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	if err := schema.Validate(&bundle); err != nil {
		t.Fatalf("generated bundle is invalid: %v", err)
	}
	for _, project := range bundle.Projects {
		for _, truthCase := range project.Truth.Cases {
			assemblyResult, err := assembly.Evaluate(project, truthCase.SelectedPartIDs)
			if err != nil {
				t.Fatalf("%s/%s assembly: %v", project.ProjectID, truthCase.CaseID, err)
			}
			actual := domain.CaseResult{
				CaseID:          truthCase.CaseID,
				SelectedPartIDs: assemblyResult.SelectedPartIDs,
				Valid:           assemblyResult.Valid,
				AcceptedPartIDs: assemblyResult.AcceptedPartIDs,
				Conflicts:       assemblyResult.Conflicts,
			}
			plan, err := planner.Plan(project, assemblyResult.AcceptedPartIDs)
			if err != nil {
				t.Fatalf("%s/%s plan: %v", project.ProjectID, truthCase.CaseID, err)
			}
			actual.Batches = plan.Batches
			actual.Decisions = plan.Decisions
			actual.CommitSequence = plan.CommitSequence
			if mismatch := diagnostics.CompareExpected(truthCase, actual); mismatch != nil {
				t.Fatalf("%s/%s: %v", project.ProjectID, truthCase.CaseID, mismatch)
			}
		}
	}
}

func TestTruthExercisesEveryTradeoffReason(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	reasons := make(map[string]bool)
	results := make(map[string]bool)
	for _, project := range bundle.Projects {
		for _, truthCase := range project.Truth.Cases {
			for _, decision := range truthCase.Decisions {
				reasons[decision.Reason] = true
				results[decision.Result] = true
			}
		}
	}
	for _, reason := range []string{
		domain.ReasonMaterialAffinity,
		domain.ReasonBudgetFit,
		domain.ReasonMaterialPressure,
		domain.ReasonVertexPressure,
		domain.ReasonIndexPressure,
		domain.ReasonSkinSeparation,
		domain.ReasonInitialBatch,
	} {
		if !reasons[reason] {
			t.Fatalf("truth does not exercise reason %s; got %v", reason, reasons)
		}
	}
	if !results[domain.DecisionReuse] || !results[domain.DecisionSplit] {
		t.Fatalf("truth does not exercise both reuse and split decisions: %v", results)
	}
}

func TestAssemblyOrderDoesNotChangeOutcome(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[0]
	selected := append([]string(nil), project.Truth.Cases[2].SelectedPartIDs...)
	want, err := assembly.Evaluate(project, selected)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	for iteration := 0; iteration < 30; iteration++ {
		shuffled := append([]string(nil), selected...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got, err := assembly.Evaluate(project, shuffled)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d changed outcome\nwant=%+v\ngot=%+v", iteration, want, got)
		}
	}
}

func TestGeneratorIsDeterministic(t *testing.T) {
	first := generator.Generate(generator.DefaultSeed)
	second := generator.Generate(generator.DefaultSeed)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seed produced different bundles")
	}
	other := generator.Generate(generator.DefaultSeed + 1)
	if reflect.DeepEqual(first, other) {
		t.Fatal("different seeds unexpectedly produced identical bundles")
	}
}

func TestMaterialVersionAndUpstreamCacheKeyPropagation(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[0]
	truthCase := project.Truth.Cases[0]
	first, err := planner.Plan(project, truthCase.AcceptedPartIDs)
	if err != nil {
		t.Fatal(err)
	}
	changed := project
	changed.Materials = append([]domain.MaterialSlot(nil), project.Materials...)
	for index := range changed.Materials {
		if changed.Materials[index].ID == "mat_cloth" {
			changed.Materials[index].Version++
		}
	}
	second, err := planner.Plan(changed, truthCase.AcceptedPartIDs)
	if err != nil {
		t.Fatal(err)
	}
	if first.Batches[0].CacheKey == second.Batches[0].CacheKey {
		t.Fatal("material version change did not alter the directly affected batch key")
	}
	for index := 1; index < len(first.Batches); index++ {
		if first.Batches[index].CacheKey == second.Batches[index].CacheKey {
			t.Fatalf("upstream cache-key chain did not invalidate %s", first.Batches[index].ID)
		}
	}
}
