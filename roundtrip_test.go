package channelpipe_test

import (
	"testing"

	"channelpipe"
	"channelpipe/internal/sample"
)

func TestExportLoadRoundTripSemanticEquivalent(t *testing.T) {
	set, err := sample.Generate(sample.Options{Tiny: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 2, 3} {
		data, loss, err := channelpipe.ExportSetAs(set.Set, version)
		if err != nil {
			t.Fatalf("export v%d: %v", version, err)
		}
		if version < 3 && !loss.HasLoss() {
			t.Fatalf("v%d expected non-semantic metadata loss record", version)
		}
		if version < 3 {
			_, _, strictErr := channelpipe.ExportSetAsStrict(set.Set, version)
			if !channelpipe.IsCode(strictErr, channelpipe.CodeLossy) {
				t.Fatalf("v%d strict export expected E_LOSSY, got %v", version, strictErr)
			}
		}
		loaded, err := channelpipe.LoadPlanSet(data)
		if err != nil {
			t.Fatalf("load exported v%d: %v", version, err)
		}
		if loaded.SourceVersion != version {
			t.Fatalf("source version = %d want %d", loaded.SourceVersion, version)
		}
		for i := range set.Set.Plans {
			ok, diff, err := channelpipe.SemanticEquivalentPlan(set.Set.Plans[i], loaded.Set.Plans[i])
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatalf("v%d semantic mismatch at %s", version, diff)
			}
		}
	}
}
