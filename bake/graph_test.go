package bake_test

import (
	"reflect"
	"testing"

	"terrain-bake/bake"
	"terrain-bake/internal/testutil"
	"terrain-bake/project"
)

func graphForTest(t *testing.T) (*project.Project, *bake.Graph) {
	t.Helper()
	dir := testutil.CopyProject(t)
	p, e := project.Load(dir)
	if e != nil {
		t.Fatal(e)
	}
	g, e := bake.NewGraph(p)
	if e != nil {
		t.Fatal(e)
	}
	return p, g
}

func TestInvalidationIsExactToDependencyComponent(t *testing.T) {
	_, g := graphForTest(t)
	report := g.Invalidate("B_0_0", "height")
	var affected []string
	for _, e := range report.Affected {
		affected = append(affected, e.BlockID)
	}
	wantAffected := []string{"B_0_0", "B_1_0", "B_0_1", "B_1_1"}
	if !reflect.DeepEqual(affected, wantAffected) {
		t.Fatalf("affected = %v, want %v", affected, wantAffected)
	}
	if !reflect.DeepEqual(report.HitBlocks, []string{"B_3_0", "B_3_1"}) {
		t.Fatalf("hit blocks = %v", report.HitBlocks)
	}
	if len(report.Constraints) != 10 {
		t.Fatalf("expected 10 bidirectional constraints, got %d", len(report.Constraints))
	}
	for _, entry := range report.Affected {
		if entry.BlockID == "B_0_0" {
			continue
		}
		if len(entry.Path) < 2 || entry.Path[0] != "B_0_0" || entry.Path[len(entry.Path)-1] != entry.BlockID {
			t.Fatalf("invalid dependency path for %s: %v", entry.BlockID, entry.Path)
		}
	}
}

func TestInvalidationOtherIslandOnlyAffectsItself(t *testing.T) {
	_, g := graphForTest(t)
	report := g.Invalidate("B_3_0", "material")
	var affected []string
	for _, e := range report.Affected {
		affected = append(affected, e.BlockID)
	}
	if !reflect.DeepEqual(affected, []string{"B_3_0", "B_3_1"}) {
		t.Fatalf("affected = %v", affected)
	}
	if !reflect.DeepEqual(report.HitBlocks, []string{"B_0_0", "B_1_0", "B_0_1", "B_1_1"}) {
		t.Fatalf("hit blocks = %v", report.HitBlocks)
	}
}

func TestDependenciesAreBidirectional(t *testing.T) {
	_, g := graphForTest(t)
	depsA := g.BlockDependencies("B_0_0")
	depsB := g.BlockDependencies("B_1_0")
	if !containsString(depsA, "B_1_0") || !containsString(depsB, "B_0_0") {
		t.Fatalf("seam dependency is not bidirectional: %v / %v", depsA, depsB)
	}
}

func TestStableDependencyCondensationOrder(t *testing.T) {
	_, g := graphForTest(t)
	first := g.OrderAll()
	for i := 0; i < 50; i++ {
		next := g.OrderAll()
		if !reflect.DeepEqual(next, first) {
			t.Fatalf("order changed: %v != %v", next, first)
		}
	}
	want := []string{"B_0_0", "B_1_0", "B_0_1", "B_1_1", "B_3_0", "B_3_1"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("order = %v, want %v", first, want)
	}
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
