package incremental_test

import (
	"testing"

	"schedlab/internal/domain"
	"schedlab/internal/gen"
	"schedlab/internal/incremental"
)

func TestIncrementalMatchesFullRecompute(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	p := b.Truth.(*domain.Project)
	eng := incremental.NewEngine(p)
	closures, stats, err := eng.ApplyEdits(b.Edits)
	if err != nil {
		t.Fatal(err)
	}
	if len(closures) != len(b.Edits) {
		t.Fatalf("closures %d want %d", len(closures), len(b.Edits))
	}
	for _, c := range closures {
		if len(c.Pods) == 0 {
			t.Fatalf("edit %s propagated to no pods", c.Source.EditKind)
		}
		if len(c.NodePairs) == 0 {
			t.Fatalf("edit %s produced no invalidated pairs", c.Source.EditKind)
		}
	}
	if stats.ReusedEntries == 0 {
		t.Fatal("expected cache reuse after targeted edits")
	}
	if stats.RecomputedEntries == 0 {
		t.Fatal("expected some recomputation after edits")
	}
	if diffs := eng.VerifyAgainstFullRecompute(); len(diffs) != 0 {
		t.Fatalf("incremental diverged from full recompute: %+v", diffs)
	}
}

func TestEditLogicalOrder(t *testing.T) {
	b, _ := gen.BuildBundle("standard")
	want := []string{"label_change", "topology_move", "pod_request", "rule_weight"}
	for i, e := range b.Edits {
		if e.Kind != want[i] {
			t.Fatalf("edit %d kind = %s want %s", i, e.Kind, want[i])
		}
	}
}
