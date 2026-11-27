package terrainbake

import (
	"context"
	"testing"
)

func TestIncrementalInvalidationAndFullEquivalence(t *testing.T) {
	p := testProject(t)
	cfg := DefaultEngineConfig()
	e, err := NewEngine(p, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Bake(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := int64(175)
	plan, err := e.ApplyEdit(Edit{BlockID: "B02", HeightDeltaMM: &h})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 3 {
		t.Fatalf("edit should invalidate B02 + 2 neighbours, got %+v", plan)
	}
	got := map[string]bool{}
	for _, iv := range plan {
		got[iv.BlockID] = true
	}
	for _, id := range []string{"B01", "B02", "B05"} {
		if !got[id] {
			t.Fatalf("missing invalidation for %s", id)
		}
	}
	for _, id := range []string{"B00", "B03", "B04"} {
		if got[id] {
			t.Fatalf("unrelated block %s was invalidated", id)
		}
	}
	inc, err := e.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inc.ReusedCount != 3 {
		t.Fatalf("reused = %d want 3 (%v)", inc.ReusedCount, inc.Reused)
	}
	for _, id := range []string{"B00", "B03", "B04"} {
		if !containsString(inc.Reused, id) {
			t.Fatalf("expected %s reused", id)
		}
	}
	for _, id := range []string{"B01", "B02", "B05"} {
		if !containsString(inc.WrittenBlocks, id) {
			t.Fatalf("expected %s rewritten", id)
		}
	}

	full, err := NewEngine(p, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := full.ApplyEdit(Edit{BlockID: "B02", HeightDeltaMM: &h}); err != nil {
		t.Fatal(err)
	}
	fullResult, err := full.BakeFull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareBakeResults(inc, fullResult); err != nil {
		t.Fatalf("incremental vs full: %v", err)
	}
}

func TestCompareReturnsFirstBlockField(t *testing.T) {
	p := testProject(t)
	e, err := NewEngine(p, DefaultEngineConfig(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bad := *r
	bad.Meshes = cloneMeshes(r.Meshes)
	bad.Meshes["B03"].Vertices[2].HeightMM++
	err = CompareBakeResults(r, &bad)
	diff := mustCode(t, err, CodeDiff)
	if diff.BlockID != "B03" || diff.Field != "vertices" || diff.LHS == "" || diff.RHS == "" {
		t.Fatalf("E_DIFF context = %+v", diff)
	}
}

func TestBudgetQueueAndThreadLimits(t *testing.T) {
	p := testProject(t)
	memCfg := EngineConfig{MaxParallelism: 3, MaxVerticesPerThread: 4096, MeshMemoryBudget: 500 << 10}
	e, err := NewEngine(p, memCfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Budget.PeakInUse > memCfg.MeshMemoryBudget {
		t.Fatalf("peak memory %d exceeds budget %d", r.Budget.PeakInUse, memCfg.MeshMemoryBudget)
	}
	if len(r.Queue) != len(p.Blocks) {
		t.Fatalf("queue events = %d want %d", len(r.Queue), len(p.Blocks))
	}
	for _, q := range r.Queue {
		if q.RequestedBytes == 0 || q.Reason == "" {
			t.Fatalf("queue event missing reason/request: %+v", q)
		}
	}

	tooSmall := EngineConfig{MaxParallelism: 2, MaxVerticesPerThread: 4096, MeshMemoryBudget: 100 << 10}
	e2, err := NewEngine(p, tooSmall, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e2.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.BlockErrors) != len(p.Blocks) {
		t.Fatalf("expected all blocks blocked, got %d errors", len(r2.BlockErrors))
	}
	if len(r2.WrittenBlocks) != 0 {
		t.Fatalf("blocked bake wrote %d blocks", len(r2.WrittenBlocks))
	}
	for _, q := range r2.Queue {
		if q.Reason != "request_exceeds_global_mesh_budget" || q.Acquired {
			t.Fatalf("queue did not record blocking reason: %+v", q)
		}
	}

	threadCfg := EngineConfig{MaxParallelism: 2, MaxVerticesPerThread: 20, MeshMemoryBudget: 8 << 20}
	e3, err := NewEngine(p, threadCfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r3, err := e3.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r3.BlockErrors) != len(p.Blocks) {
		t.Fatalf("per-thread cap should fail all blocks, got %d", len(r3.BlockErrors))
	}
	for _, be := range r3.BlockErrors {
		if be.Code != CodeBlock || be.Field != "max_vertices_per_thread" {
			t.Fatalf("unexpected thread cap error: %+v", be)
		}
	}
}

func TestSingleBlockFailureIsolation(t *testing.T) {
	p := testProject(t)
	e, err := NewEngine(p, DefaultEngineConfig(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Bake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.SetFault("B02", "block"); err != nil {
		t.Fatal(err)
	}
	r, err := e.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	be := r.BlockErrors["B02"]
	if be == nil || be.Code != CodeBlock {
		t.Fatalf("B02 error = %+v", be)
	}
	if len(r.BlockErrors) != 1 {
		t.Fatalf("failure leaked to %v", r.BlockErrorIDs())
	}
	if len(r.Meshes) != len(p.Blocks)-1 {
		t.Fatalf("available committed meshes = %d, want %d", len(r.Meshes), len(p.Blocks)-1)
	}
	for _, id := range []string{"B00", "B01", "B03", "B04", "B05"} {
		if r.Meshes[id] == nil {
			t.Fatalf("committed block %s lost mesh", id)
		}
	}
	if len(r.CommitSequence) != len(p.Blocks)-1 {
		t.Fatalf("commit sequence = %v", r.CommitSequence)
	}

	if err := e.SetFault("B01", "decode"); err != nil {
		t.Fatal(err)
	}
	r2, err := e.Bake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r2.BlockErrors["B01"] == nil || r2.BlockErrors["B01"].Code != CodeDecode {
		t.Fatalf("decode fault = %+v", r2.BlockErrors["B01"])
	}
}
