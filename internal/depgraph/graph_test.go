package depgraph_test

import (
	"reflect"
	"testing"

	"terrainpipeline/internal/depgraph"
	"terrainpipeline/internal/model"
	"terrainpipeline/internal/samplegen"
)

func TestExactInvalidationScope(t *testing.T) {
	project := samplegen.Generate(model.FixedSeed)
	graph := depgraph.Build(project)
	cached := map[string]bool{}
	for _, id := range project.SortedBlockIDs() {
		cached[id] = true
	}

	elevation := graph.Plan([]depgraph.Edit{{Type: "elevation", BlockID: "B01"}}, cached)
	assertStrings(t, "elevation invalidated", elevation.Invalidated, []string{"B01", "B02"})
	assertStrings(t, "elevation reused", elevation.Reused, []string{"B03", "B04", "B05", "B06", "B07"})

	material := graph.Plan([]depgraph.Edit{{Type: "material", LayerID: "rock"}}, cached)
	assertStrings(t, "material invalidated", material.Invalidated, []string{"B01", "B02", "B03", "B04", "B05"})
	assertStrings(t, "material reused", material.Reused, []string{"B06", "B07"})

	operator := graph.Plan([]depgraph.Edit{{Type: "operator"}}, cached)
	assertStrings(t, "operator invalidated", operator.Invalidated, project.SortedBlockIDs())

	if deps := graph.BlockDependencies["B01"]; !reflect.DeepEqual(deps, []string{"B02"}) {
		t.Fatalf("expected bidirectional seam dependency B01<->B02, got %v", deps)
	}
	if deps := graph.BlockDependencies["B02"]; !reflect.DeepEqual(deps, []string{"B01", "B03"}) {
		t.Fatalf("expected B02 dependencies B01,B03, got %v", deps)
	}
}

func assertStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %v want %v", name, got, want)
	}
}
