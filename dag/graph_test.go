package dag_test

import (
	"testing"

	"dialoguefit/dag"
)

func TestGraphPropagatesToAlignedAndComposite(t *testing.T) {
	g := dag.New()
	g.Add(dag.Node{Key: "env", Kind: "envelope", PhonemeIndex: 3})
	g.Add(dag.Node{Key: "coef", Kind: "stretch_coefficient", PhonemeIndex: 3})
	g.Add(dag.Node{Key: "range", Kind: "stretch_range", PhonemeIndex: 3, DependsOn: []string{"coef"}})
	g.Add(dag.Node{Key: "stress", Kind: "stress", PhonemeIndex: 3, DependsOn: []string{"env"}})
	g.Add(dag.Node{Key: "aligned", Kind: "aligned_stress", PhonemeIndex: 3, DependsOn: []string{"stress", "range"}})
	g.Add(dag.Node{Key: "result", Kind: "phoneme_result", PhonemeIndex: 3, DependsOn: []string{"env", "stress", "coef", "range", "aligned"}})

	invalid := g.Invalidate("coef")
	assertContains(t, invalid, "coef", "range", "aligned", "result")
	assertNotContains(t, invalid, "env", "stress")

	invalid = g.Invalidate("env")
	assertContains(t, invalid, "env", "stress", "aligned", "result")
	assertNotContains(t, invalid, "coef", "range")

	order := g.TopologicalOrder(g.Invalidate("coef"))
	if len(order) != 4 {
		t.Fatalf("order len=%d", len(order))
	}
	positions := map[string]int{}
	for i, n := range order {
		positions[n.Key] = i
	}
	if positions["coef"] >= positions["range"] || positions["range"] >= positions["aligned"] {
		t.Fatalf("bad topology: %v", positions)
	}
}

func assertContains(t *testing.T, keys []string, want ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	for _, k := range want {
		if !set[k] {
			t.Fatalf("expected %q in %v", k, keys)
		}
	}
}

func assertNotContains(t *testing.T, keys []string, unwanted ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	for _, k := range unwanted {
		if set[k] {
			t.Fatalf("did not expect %q in %v", k, keys)
		}
	}
}
