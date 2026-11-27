package terrainbake

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

func buildOne(t *testing.T, p *Project, b BlockSpec, gen *Generator) *Mesh {
	t.Helper()
	local := func(x, y int64) int64 { return gen.HeightMM(b.ID, b.Rect, 0, x, y) }
	m, err := BuildMesh(p, b, local, gen.BaseHeightMM)
	if err != nil {
		t.Fatalf("BuildMesh %s: %v", b.ID, err)
	}
	return m
}

func TestMeshDeterministicAcrossConcurrentBuilds(t *testing.T) {
	p := testProject(t)
	b := findBlock(t, p, "B04")
	gen := NewGenerator(p.World.Seed)
	base := buildOne(t, p, b, gen)
	const workers = 12
	var wg sync.WaitGroup
	results := make([]*Mesh, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			local := func(x, y int64) int64 { return gen.HeightMM(b.ID, b.Rect, 0, x, y) }
			results[idx], errs[idx] = BuildMesh(p, b, local, gen.BaseHeightMM)
		}(i)
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if !reflect.DeepEqual(base.Vertices, results[i].Vertices) {
			t.Fatalf("worker %d vertex order/content differs", i)
		}
		if !reflect.DeepEqual(base.Triangles, results[i].Triangles) {
			t.Fatalf("worker %d triangle order/content differs", i)
		}
		if base.VertexDigest != results[i].VertexDigest || base.TriangleDigest != results[i].TriangleDigest {
			t.Fatalf("worker %d digest differs", i)
		}
	}
}

func TestMeshTruthBudgetsAndWorkerIndependence(t *testing.T) {
	p := testProject(t)
	configs := []EngineConfig{
		{MaxParallelism: 1, MaxVerticesPerThread: 4096, MeshMemoryBudget: 8 << 20},
		{MaxParallelism: 4, MaxVerticesPerThread: 4096, MeshMemoryBudget: 8 << 20},
	}
	var results []*BakeResult
	for _, cfg := range configs {
		e, err := NewEngine(p, cfg, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		r, err := e.Bake(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
		if r.TruthCheck == nil || len(r.TruthCheck.Failures) > 0 {
			t.Fatalf("truth check failed: %#v", r.TruthCheck)
		}
		if r.TruthCheck.SamplesChecked != len(p.Truth.Samples) {
			t.Fatalf("samples checked = %d want %d", r.TruthCheck.SamplesChecked, len(p.Truth.Samples))
		}
		if r.TruthCheck.SeamsChecked != len(p.Seams) {
			t.Fatalf("seams checked = %d want %d", r.TruthCheck.SeamsChecked, len(p.Seams))
		}
		for _, b := range p.Blocks {
			m := r.Meshes[b.ID]
			if m == nil {
				t.Fatalf("missing mesh %s", b.ID)
			}
			if len(m.Vertices) > b.Budget.MaxVertices {
				t.Fatalf("%s vertices %d > budget %d", b.ID, len(m.Vertices), b.Budget.MaxVertices)
			}
			if len(m.Triangles) > b.Budget.MaxTriangles {
				t.Fatalf("%s triangles %d > budget %d", b.ID, len(m.Triangles), b.Budget.MaxTriangles)
			}
			if p.Truth.MaxTriangles[b.ID] != b.Budget.MaxTriangles {
				t.Fatalf("%s truth triangle cap disagrees", b.ID)
			}
		}
	}
	if err := CompareBakeResults(results[0], results[1]); err != nil {
		t.Fatalf("worker configs differ: %v", err)
	}
}

func TestMeshParameterRanges(t *testing.T) {
	p := testProject(t)
	b := findBlock(t, p, "B00")
	gen := NewGenerator(p.World.Seed)
	local := func(x, y int64) int64 { return gen.BaseHeightMM(x, y) }

	bad := b
	bad.Budget.MaxVertices = 1
	_, err := BuildMesh(p, bad, local, gen.BaseHeightMM)
	e := mustCode(t, err, CodePara)
	if e.Field != "budget.max_vertices" || e.Legal == "" {
		t.Fatalf("E_PARA budget context: %+v", e)
	}

	bad = b
	bad.ToleranceUM = 0
	_, err = BuildMesh(p, bad, local, gen.BaseHeightMM)
	e = mustCode(t, err, CodePara)
	if e.Field != "chord_tolerance" || e.Legal == "" {
		t.Fatalf("E_PARA tolerance context: %+v", e)
	}
}
