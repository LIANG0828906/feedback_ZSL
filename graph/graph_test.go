package graph_test

import (
	"reflect"
	"testing"

	"iconflow/generator"
	"iconflow/graph"
)

func TestImpactManifestOrderIndependent(t *testing.T) {
	project := generator.GenerateProject(generator.FixedSeed)
	g := graph.ProjectGraph(project)
	first := g.Impact([]string{"channel_order", "threshold_scale"}, nil)
	second := g.Impact([]string{"threshold_scale", "channel_order"}, nil)
	if len(first.Items) != len(second.Items) || len(first.Blocks) != len(second.Blocks) {
		t.Fatalf("manifest sizes differ: %d/%d blocks %d/%d", len(first.Items), len(second.Items), len(first.Blocks), len(second.Blocks))
	}
	for i := range first.Items {
		if !reflect.DeepEqual(first.Items[i], second.Items[i]) {
			t.Fatalf("item %d differs:\n%+v\n%+v", i, first.Items[i], second.Items[i])
		}
	}
	if len(first.Blocks) != len(project.SortedBlockCoords()) {
		t.Fatalf("global edits should affect every block, got %d", len(first.Blocks))
	}
}

func TestScopedImpactOnlyTargetBlock(t *testing.T) {
	project := generator.GenerateProject(generator.FixedSeed)
	target := project.SortedBlockCoords()[3].Key()
	manifest := graph.ProjectGraph(project).Impact([]string{"threshold_scale"}, map[string]bool{target: true})
	if len(manifest.Blocks) != 1 || manifest.Blocks[0] != target {
		t.Fatalf("expected only %s, got %+v", target, manifest.Blocks)
	}
	foundStats := false
	for _, item := range manifest.Items {
		if item.InvalidatedNode == "block/"+target+"/stats" {
			foundStats = true
			if len(item.DependencyPath) < 4 {
				t.Fatalf("dependency path too short: %+v", item.DependencyPath)
			}
		}
	}
	if !foundStats {
		t.Fatal("stats downstream impact missing")
	}
}
