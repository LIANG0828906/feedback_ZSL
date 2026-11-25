package assembly_test

import (
	"reflect"
	"testing"

	"wardrobe/assembly"
	"wardrobe/domain"
	"wardrobe/generator"
)

func TestConflictCategoriesAndAcceptedSubset(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[0]
	truthCase := project.Truth.Cases[2]
	before := domain.HashJSON(project)
	result, err := assembly.Evaluate(project, truthCase.SelectedPartIDs)
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid {
		t.Fatal("conflicting pose was reported valid")
	}
	if !reflect.DeepEqual(result.AcceptedPartIDs, truthCase.AcceptedPartIDs) {
		t.Fatalf("accepted=%v truth=%v", result.AcceptedPartIDs, truthCase.AcceptedPartIDs)
	}
	categories := make(map[string]int)
	for _, conflict := range result.Conflicts {
		categories[conflict.Category]++
		if conflict.Message == "" {
			t.Fatal("empty conflict explanation")
		}
	}
	for _, category := range []string{
		domain.ConflictMountContention,
		domain.ConflictExplicitExclude,
		domain.ConflictSkinIncompatible,
	} {
		if categories[category] == 0 {
			t.Fatalf("missing conflict category %s: %+v", category, result.Conflicts)
		}
	}
	if domain.HashJSON(project) != before {
		t.Fatal("pure assembly evaluation mutated the project")
	}
}

func TestKnownValidPose(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[3]
	truthCase := project.Truth.Cases[0]
	result, err := assembly.Evaluate(project, truthCase.SelectedPartIDs)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid {
		t.Fatalf("expected valid pose, conflicts=%+v", result.Conflicts)
	}
	if !reflect.DeepEqual(result.AcceptedPartIDs, truthCase.AcceptedPartIDs) {
		t.Fatalf("accepted=%v truth=%v", result.AcceptedPartIDs, truthCase.AcceptedPartIDs)
	}
}
