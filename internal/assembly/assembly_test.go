package assembly_test

import (
	"math/rand"
	"reflect"
	"testing"

	"wardrobekernel/internal/assembly"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/samplegen"
)

func TestEvaluateIsOrderIndependentAndCategorizesConflicts(t *testing.T) {
	project, err := samplegen.Generate(model.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}

	first, err := assembly.Evaluate(project, project.CandidateAssembly)
	if err != nil {
		t.Fatal(err)
	}

	ids := append([]string(nil), project.CandidateAssembly...)
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 20; i++ {
		rng.Shuffle(len(ids), func(a, b int) { ids[a], ids[b] = ids[b], ids[a] })
		shuffled, err := assembly.Evaluate(project, ids)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, shuffled) {
			t.Fatalf("shuffle %d changed assembly result", i)
		}
	}

	categories := map[string]bool{}
	for _, conflict := range first.Conflicts {
		categories[conflict.Category] = true
		if conflict.WinnerID == "" || conflict.LoserID == "" {
			t.Fatalf("conflict must identify winner/loser: %#v", conflict)
		}
	}
	for _, category := range []string{model.ConflictAttachPoint, model.ConflictMutex, model.ConflictSkin} {
		if !categories[category] {
			t.Fatalf("missing conflict category %s in %#v", category, categories)
		}
	}
	if len(first.LegalAssembly) != 9 {
		t.Fatalf("expected 9 legal parts, got %v", first.LegalAssembly)
	}
}
