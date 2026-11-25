package topology_test

import (
	"testing"

	"skeletonretarget/internal/model"
	"skeletonretarget/internal/topology"
)

func TestMissingParentAndCycleNameFirstOffendingJoint(t *testing.T) {
	missing := []model.Joint{
		{ID: "root"},
		{ID: "child", Parent: "ghost"},
	}
	_, err := topology.SortSkeleton(missing)
	if !model.IsCode(err, model.CodeTopo) || err.Joint != "child" {
		t.Fatalf("expected E_TOPO at child, got %v", err)
	}

	cycle := []model.Joint{
		{ID: "a", Parent: "c"},
		{ID: "b", Parent: "a"},
		{ID: "c", Parent: "b"},
	}
	_, err = topology.SortSkeleton(cycle)
	if !model.IsCode(err, model.CodeTopo) || err.Joint == "" {
		t.Fatalf("expected E_TOPO with offending joint, got %v", err)
	}
}

func TestFlatOrderSortsParentBeforeChild(t *testing.T) {
	joints := []model.Joint{
		{ID: "hand", Parent: "arm"},
		{ID: "arm", Parent: "root"},
		{ID: "root"},
	}
	sorted, err := topology.SortSkeleton(joints)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{sorted[0].ID, sorted[1].ID, sorted[2].ID}
	want := []string{"root", "arm", "hand"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted order %v, want %v", got, want)
		}
	}
}
