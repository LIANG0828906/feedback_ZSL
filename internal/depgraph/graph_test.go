package depgraph_test

import (
	"testing"

	"wardrobekernel/internal/batching"
	"wardrobekernel/internal/depgraph"
	"wardrobekernel/internal/model"
	"wardrobekernel/internal/samplegen"
)

func TestMaterialAndAttachNodesHaveBatchEdges(t *testing.T) {
	project, err := samplegen.Generate(model.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := batching.Plan(project)
	if err != nil {
		t.Fatal(err)
	}
	graph := depgraph.Enrich(project, plan)

	material := graph.Nodes["material:waist_belt:main"]
	if material == nil {
		t.Fatal("missing waist_belt material node")
	}
	if !contains(material.Children, "batch:3") {
		t.Fatalf("material node children=%v, want batch:3", material.Children)
	}
	attach := graph.Nodes["attach:waist"]
	if attach == nil || !contains(attach.Children, "part:waist_belt") {
		t.Fatalf("attach occupancy edge missing: %#v", attach)
	}
	batch3 := graph.Nodes["batch:3"]
	if batch3 == nil || !contains(batch3.Parents, "material:waist_belt:main") {
		t.Fatalf("batch parents missing material dependency: %#v", batch3)
	}

	inv := graph.Propagate([]string{"waist_belt"}, false)
	if len(inv.InvalidBatches) != 1 || inv.InvalidBatches[0] != 3 {
		t.Fatalf("invalid batches=%v", inv.InvalidBatches)
	}
	if !contains(inv.InvalidNodes, "material:waist_belt:main") || !contains(inv.InvalidNodes, "offset:3") {
		t.Fatalf("propagation missed material/offset nodes: %v", inv.InvalidNodes)
	}
}

func contains(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}
