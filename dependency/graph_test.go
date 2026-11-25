package dependency_test

import (
	"reflect"
	"testing"

	"wardrobe/assembly"
	"wardrobe/dependency"
	"wardrobe/domain"
	"wardrobe/generator"
	"wardrobe/planner"
)

func TestDependencyGraphAndDownstreamPropagation(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	project := bundle.Projects[0]
	truthCase := project.Truth.Cases[0]
	evaluated, err := assembly.Evaluate(project, truthCase.SelectedPartIDs)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Plan(project, evaluated.AcceptedPartIDs)
	if err != nil {
		t.Fatal(err)
	}
	result := domain.CaseResult{
		SelectedPartIDs: evaluated.SelectedPartIDs,
		AcceptedPartIDs: evaluated.AcceptedPartIDs,
		Batches:         plan.Batches,
		Decisions:       plan.Decisions,
		CommitSequence:  plan.CommitSequence,
	}
	graph := dependency.Build(project, result)
	kinds := make(map[string]int)
	for _, node := range graph.Nodes {
		kinds[node.Kind]++
	}
	for _, kind := range []string{
		dependency.KindMount, dependency.KindPart, dependency.KindMaterial,
		dependency.KindBudget, dependency.KindBatch, dependency.KindSubmesh,
	} {
		if kinds[kind] == 0 {
			t.Fatalf("dependency graph has no %s node: %+v", kind, kinds)
		}
	}
	affected := dependency.AffectedBatches(graph, []string{"hand_l_glove"}, nil)
	if !reflect.DeepEqual(affected, []string{"B003", "B004"}) {
		t.Fatalf("unexpected downstream batches: %v", affected)
	}
	affectedByMaterial := dependency.AffectedBatches(graph, nil, []string{"mat_cloth"})
	if !reflect.DeepEqual(affectedByMaterial, []string{"B001", "B002", "B003", "B004"}) {
		t.Fatalf("unexpected material propagation: %v", affectedByMaterial)
	}
}
