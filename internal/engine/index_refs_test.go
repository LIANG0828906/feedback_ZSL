package engine_test

import (
	"testing"

	"graphlab/internal/engine"
	"graphlab/internal/model"
)

func TestSharedIndexKeyReferenceLifecycle(t *testing.T) {
	p := &model.Project{
		ID: "refs", Version: 1, Seed: 7, HierarchyEnabled: false,
		ClockUnit: "ms", CapacityUnit: "byte", ObservedAtMillis: 10, CacheCapacityByte: 1024,
		Shards: []model.Shard{
			{ID: "s0", Low: "v0000", High: "v0002"},
			{ID: "s1", Low: "v0002", High: "v0004"},
		},
		Vertices: []model.Vertex{
			{ID: "v0000", DeclOrder: 0, Labels: []string{"blue"}},
			{ID: "v0001", DeclOrder: 1, Labels: []string{"blue"}},
			{ID: "v0002", DeclOrder: 2, Labels: []string{"blue"}},
			{ID: "v0003", DeclOrder: 3, Labels: []string{"blue"}},
		},
	}
	if err := model.AssignShards(p); err != nil {
		t.Fatal(err)
	}
	rt, err := engine.NewRuntime(p, engine.DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	blue := engine.LabelKey("blue")
	assertKeyState(t, rt, blue, []string{"s0", "s1"}, 1)

	for i := 0; i < 3; i++ {
		v := model.CloneVertex(rt.Project.Vertices[i])
		v.Labels = []string{"core"}
		op := model.Operation{Seq: i + 1, Kind: model.OpVertexUpdate, VertexID: v.ID, Vertex: &v}
		rep, err := rt.ApplyOperation(op, engine.LayoutIndex)
		if err != nil {
			t.Fatal(err)
		}
		if !contains(rep.AffectedIndexKeys, blue) {
			t.Fatalf("operation did not report shared key %s", blue)
		}
		wantRefs := []string{"s0", "s1"}
		if i == 1 {
			wantRefs = []string{"s1"}
		}
		if i == 2 {
			wantRefs = []string{"s1"}
		}
		assertKeyState(t, rt, blue, wantRefs, i+2)
	}

	v := model.CloneVertex(rt.Project.Vertices[3])
	v.Labels = []string{"core"}
	rep, err := rt.ApplyOperation(model.Operation{
		Seq: 4, Kind: model.OpVertexUpdate, VertexID: v.ID, Vertex: &v,
	}, engine.LayoutIndex)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Index.Keys[blue] != nil {
		t.Fatalf("shared key %s should be reclaimed after last reference disappears", blue)
	}
	if !contains(rep.AffectedIndexKeys, blue) {
		t.Fatalf("last-reference reclamation was not reported")
	}
}

func TestReshardRetainsKeyVersionBase(t *testing.T) {
	p := &model.Project{
		ID: "reshard-version", Version: 1, Seed: 8, HierarchyEnabled: false,
		ClockUnit: "ms", CapacityUnit: "byte", ObservedAtMillis: 10, CacheCapacityByte: 1024,
		Shards: []model.Shard{
			{ID: "s0", Low: "v0000", High: "v0002"},
			{ID: "s1", Low: "v0002", High: "v0004"},
		},
		Vertices: []model.Vertex{
			{ID: "v0000", DeclOrder: 0, Labels: []string{"green"}},
			{ID: "v0001", DeclOrder: 1, Labels: []string{"green"}},
			{ID: "v0002", DeclOrder: 2, Labels: []string{"blue"}},
			{ID: "v0003", DeclOrder: 3, Labels: []string{"blue"}},
		},
	}
	if err := model.AssignShards(p); err != nil {
		t.Fatal(err)
	}
	rt, err := engine.NewRuntime(p, engine.DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	blue := engine.LabelKey("blue")
	assertKeyState(t, rt, blue, []string{"s1"}, 1)
	report, err := rt.ApplyOperation(model.Operation{
		Seq: 1, Kind: model.OpReshard,
		Shards: []model.Shard{
			{ID: "s0", Low: "v0000", High: "v0002"},
			{ID: "s1a", Low: "v0002", High: "v0003"},
			{ID: "s1b", Low: "v0003", High: "v0004"},
		},
	}, engine.LayoutIndex)
	if err != nil {
		t.Fatal(err)
	}
	assertKeyState(t, rt, blue, []string{"s1a", "s1b"}, 2)
	if !contains(report.AffectedIndexKeys, blue) {
		t.Fatalf("reshard did not report affected key %s", blue)
	}
}

func assertKeyState(t *testing.T, rt *engine.Runtime, key string, refs []string, version int) {
	t.Helper()
	ik := rt.Index.Keys[key]
	if ik == nil {
		t.Fatalf("key %s was reclaimed too early", key)
	}
	if !equal(ik.RefShards, refs) {
		t.Fatalf("key %s refs got=%v want=%v", key, ik.RefShards, refs)
	}
	if ik.Version != version {
		t.Fatalf("key %s version got=%d want=%d", key, ik.Version, version)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
