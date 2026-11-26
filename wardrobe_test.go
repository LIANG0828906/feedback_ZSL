package wardrobe_test

import (
	"testing"

	wardrobe "wardrobekernel"
)

func TestPublicFacade(t *testing.T) {
	project, err := wardrobe.GenerateSample(wardrobe.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := wardrobe.EvaluateAssembly(project, project.CandidateAssembly)
	if err != nil {
		t.Fatal(err)
	}
	if len(assembly.Conflicts) == 0 || len(assembly.LegalAssembly) == 0 {
		t.Fatal("expected conflicts and a non-empty legal assembly")
	}
	plan, err := wardrobe.FullPlan(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Batches) != 4 || plan.CommitSequence[0] != 1 {
		t.Fatalf("unexpected plan: %#v", plan.CommitSequence)
	}
}
