package dag_test

import (
	"testing"

	"prosodyalign/internal/dag"
)

func TestCacheKeyIncludesAllDeterministicInputs(t *testing.T) {
	k1 := dag.CacheKey("hash-a", "p1", "v1", dag.NodeEnvelope, "up1")
	k2 := dag.CacheKey("hash-a", "p1", "v1", dag.NodeEnvelope, "up1")
	k3 := dag.CacheKey("hash-b", "p1", "v1", dag.NodeEnvelope, "up1")
	k4 := dag.CacheKey("hash-a", "p2", "v1", dag.NodeEnvelope, "up1")
	if k1 != k2 || k1 == k3 || k1 == k4 {
		t.Fatalf("cache keys must be stable and input-sensitive: %s %s %s %s", k1, k2, k3, k4)
	}
}

func TestInvalidationPropagatesOnlyToDownstream(t *testing.T) {
	g := dag.NewGraph()
	g.Add(dag.Node{Key: "env1", Kind: dag.NodeEnvelope})
	g.Add(dag.Node{Key: "stretch1", Kind: dag.NodeStretch})
	g.Add(dag.Node{Key: "range1", Kind: dag.NodeRange, DependsOn: []string{"stretch1"}})
	g.Add(dag.Node{Key: "range2", Kind: dag.NodeRange, DependsOn: []string{"stretch2", "range1"}})
	g.Add(dag.Node{Key: "stretch2", Kind: dag.NodeStretch})
	g.Add(dag.Node{Key: "accent1", Kind: dag.NodeAccent, DependsOn: []string{"env1"}})
	invalid := g.Invalidate("stretch1")
	has := map[string]bool{}
	for _, k := range invalid {
		has[k] = true
	}
	if !has["stretch1"] || !has["range1"] || !has["range2"] {
		t.Fatalf("expected serial range propagation, got %v", invalid)
	}
	if has["env1"] || has["accent1"] || has["stretch2"] {
		t.Fatalf("upstream/unrelated nodes invalidated: %v", invalid)
	}
}
