package generator

import (
	"testing"

	"graphlab/internal/engine"
	"graphlab/internal/loader"
	"graphlab/internal/model"
)

func TestThreeSchemasMatchTruthAndEngine(t *testing.T) {
	sample, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := loader.LoadBundle(sample.Documents)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.CoreDigest != sample.Truth.CoreDigest {
		t.Fatalf("core digest mismatch: bundle=%s truth=%s", bundle.CoreDigest, sample.Truth.CoreDigest)
	}
	if bundle.Projects[model.SchemaV3].HierarchyEnabled {
		t.Fatal("v3 optional hierarchy should be marked disabled")
	}
	if !bundle.Projects[model.SchemaV3].HierarchyDegraded {
		t.Fatal("v3 optional hierarchy should be marked degraded")
	}
	if !bundle.Projects[model.SchemaV1].HierarchyEnabled || !bundle.Projects[model.SchemaV2].HierarchyEnabled {
		t.Fatal("v1/v2 hierarchy should be enabled")
	}

	rt, err := engine.NewRuntime(bundle.Projects[model.SchemaV1], engine.DefaultBuildOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertShardTruth(t, rt, sample.Truth)
	results, err := rt.EvaluateAll(engine.LayoutIndex)
	if err != nil {
		t.Fatal(err)
	}
	assertQueryTruth(t, results, sample.Truth.Queries)
}

func TestGeneratorDeterminism(t *testing.T) {
	a, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for version, docA := range a.Documents {
		if string(docA) != string(b.Documents[version]) {
			t.Fatalf("%s document differs between identical generator runs", version)
		}
	}
}

func assertShardTruth(t *testing.T, rt *engine.Runtime, truth *model.Truth) {
	t.Helper()
	for shardID, want := range truth.Shards {
		si := rt.Index.Shards[shardID]
		if si == nil {
			t.Fatalf("missing shard index %s", shardID)
		}
		gotVertices := append([]string(nil), si.VertexOrder...)
		gotEdges := append([]string(nil), si.EdgeOrder...)
		if !sameStrings(gotVertices, want.VertexOrder) {
			t.Fatalf("vertex order mismatch for %s: got=%v want=%v", shardID, gotVertices, want.VertexOrder)
		}
		if !sameStrings(gotEdges, want.EdgeOrder) {
			t.Fatalf("edge order mismatch for %s: got=%v want=%v", shardID, gotEdges, want.EdgeOrder)
		}
		gotKeys := rt.Index.ShardKeys(shardID)
		if !sameStrings(gotKeys, want.IndexKeys) {
			t.Fatalf("index key mismatch for %s: got=%v want=%v", shardID, gotKeys, want.IndexKeys)
		}
	}
}

func assertQueryTruth(t *testing.T, got []model.QueryResult, want map[string]model.QueryTruth) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("query result count got=%d want=%d", len(got), len(want))
	}
	for _, result := range got {
		w := want[result.QueryID]
		if result.Status != w.Status ||
			!sameStrings(result.VisitedVertices, w.VisitedVertices) ||
			!sameStrings(result.VisitedEdges, w.VisitedEdges) ||
			!sameStrings(result.ResultVertices, w.ResultVertices) ||
			!sameStrings(result.PathVertices, w.PathVertices) ||
			!sameStrings(result.PathEdges, w.PathEdges) {
			t.Fatalf("query %s mismatch\n got=%+v\nwant=%+v", result.QueryID, result, w)
		}
	}
}

func sameStrings(a, b []string) bool {
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
