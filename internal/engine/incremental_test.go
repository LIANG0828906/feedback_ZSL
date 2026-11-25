package engine_test

import (
	"sort"
	"testing"

	"graphlab/internal/engine"
	"graphlab/internal/generator"
	"graphlab/internal/loader"
	"graphlab/internal/model"
)

func TestConcurrencyLayoutAndIncrementalRebuild(t *testing.T) {
	sample, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := loader.LoadBundle(sample.Documents)
	if err != nil {
		t.Fatal(err)
	}
	base := bundle.Projects[model.SchemaV1]

	one, err := engine.NewRuntime(base, engine.BuildOptions{Workers: 1, BufferPerWorker: 4096})
	if err != nil {
		t.Fatal(err)
	}
	many, err := engine.NewRuntime(base, engine.BuildOptions{Workers: 4, BufferPerWorker: 4096})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotEqual(t, one, many)
	indexResults, err := one.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		t.Fatal(err)
	}
	scanResults, err := many.EvaluateAll(engine.LayoutScan)
	if err != nil {
		t.Fatal(err)
	}
	if diffs := engine.CompareQueryResults(indexResults, scanResults); len(diffs) > 0 {
		t.Fatalf("layout mismatch: %v", diffs)
	}

	for i, op := range base.Operations {
		report, err := one.ApplyOperation(op, engine.LayoutIndex)
		if err != nil {
			t.Fatalf("operation %d failed: %v", op.Seq, err)
		}
		truth := sample.Truth.Operations[i]
		if report.AffectedShards == nil {
			report.AffectedShards = []string{}
		}
		assertStringSet(t, "affected shards op "+op.Kind, report.AffectedShards, truth.AffectedShards)
		assertStringSet(t, "changed entities op "+op.Kind, report.ChangedEntities, truth.ChangedEntities)
		assertStringSet(t, "affected keys op "+op.Kind, report.AffectedIndexKeys, truth.AffectedIndexKeys)
		assertStringSet(t, "invalidated caches op "+op.Kind, cacheInvalidationIDs(report.InvalidatedCaches), truth.InvalidatedCacheIDs)
		assertStringSet(t, "reused caches op "+op.Kind, report.ReusedCacheIDs, truth.ReusedCacheIDs)
		assertQueryTruth(t, report.QueryResults, truth.Queries)

		fresh, fullResults, err := one.FullRebuild(engine.LayoutIndex)
		if err != nil {
			t.Fatal(err)
		}
		if diffs := engine.CompareQueryResults(report.QueryResults, fullResults); len(diffs) > 0 {
			t.Fatalf("incremental != full after op %d (%s): %v", op.Seq, op.Kind, diffs)
		}
		assertKeySet(t, one, fresh)
	}
}

func TestBackpressureCodes(t *testing.T) {
	sample, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := loader.LoadBundle(sample.Documents)
	if err != nil {
		t.Fatal(err)
	}
	base := bundle.Projects[model.SchemaV1]
	if _, err := engine.NewRuntime(base, engine.BuildOptions{Workers: 0, BufferPerWorker: 64}); !model.IsCode(err, model.EThreadBudget) {
		t.Fatalf("expected E_THREAD_BUDGET, got %v", err)
	}
	if _, err := engine.NewRuntime(base, engine.BuildOptions{Workers: 2, BufferPerWorker: 1}); !model.IsCode(err, model.EBufferBudget) {
		t.Fatalf("expected E_BUFFER_BUDGET, got %v", err)
	}
}

func assertSnapshotEqual(t *testing.T, a, b *engine.Runtime) {
	t.Helper()
	if model.CoreDigest(a.Project) != model.CoreDigest(b.Project) {
		t.Fatal("core digest differs")
	}
	if len(a.Index.Shards) != len(b.Index.Shards) || len(a.Index.Keys) != len(b.Index.Keys) {
		t.Fatal("index size differs")
	}
	for id, si := range a.Index.Shards {
		other := b.Index.Shards[id]
		if other == nil || !equal(si.VertexOrder, other.VertexOrder) ||
			!equal(si.EdgeOrder, other.EdgeOrder) || !equal(a.Index.ShardKeys(id), b.Index.ShardKeys(id)) {
			t.Fatalf("shard %s differs", id)
		}
	}
	for _, kv := range a.Index.KeyViews() {
		other := b.Index.Keys[kv.Key]
		if other == nil || !equal(kv.RefShards, other.RefShards) || !equal(kv.Entities, other.Entities) {
			t.Fatalf("key %s differs", kv.Key)
		}
	}
}

func assertKeySet(t *testing.T, incremental, fresh *engine.Runtime) {
	t.Helper()
	if len(incremental.Index.Keys) != len(fresh.Index.Keys) {
		t.Fatalf("incremental key count=%d full key count=%d", len(incremental.Index.Keys), len(fresh.Index.Keys))
	}
	for key, ik := range fresh.Index.Keys {
		got := incremental.Index.Keys[key]
		if got == nil || !equal(got.RefShards, ik.RefShards) || !equal(got.Entities, ik.Entities) {
			t.Fatalf("key %s refs/entities differ between incremental and full rebuild", key)
		}
	}
}

func assertQueryTruth(t *testing.T, got []model.QueryResult, want map[string]model.QueryTruth) {
	t.Helper()
	for _, result := range got {
		w := want[result.QueryID]
		if result.Status != w.Status || !equal(result.VisitedVertices, w.VisitedVertices) ||
			!equal(result.VisitedEdges, w.VisitedEdges) ||
			!equal(result.ResultVertices, w.ResultVertices) ||
			!equal(result.PathVertices, w.PathVertices) || !equal(result.PathEdges, w.PathEdges) {
			t.Fatalf("query %s mismatch\n got=%+v\nwant=%+v", result.QueryID, result, w)
		}
	}
}

func cacheInvalidationIDs(in []engine.CacheInvalidation) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		out = append(out, item.CacheID)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertStringSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	gg := append([]string(nil), got...)
	ww := append([]string(nil), want...)
	sort.Strings(gg)
	sort.Strings(ww)
	if !equal(gg, ww) {
		t.Fatalf("%s mismatch\ngot=%v\nwant=%v", label, gg, ww)
	}
}
