package store

import (
	"context"
	"reflect"
	"testing"

	"terrainkernel/internal/bake"
	"terrainkernel/internal/gen"
	"terrainkernel/internal/model"
)

func TestCheckpointCommitAndResume(t *testing.T) {
	samples := storeSamples(t)
	path := t.TempDir() + "/checkpoint.db"
	fp := BuildFingerprint(samples.Project)
	graph := bake.BuildGraph(samples.Project)
	db, err := Open(path, fp)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := bake.NewEngine(samples.Project, graph,
		bake.EngineConfig{MaxWorkers: 4, BufferBudgetPx: 32, StopAfter: -1},
		NewCommitter(db))
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.Run(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.CommitSequence, []int{0, 1, 2, 3}) {
		t.Fatalf("commit sequence = %v", first.CommitSequence)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path, fp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	loaded, sequence, resetReason, err := db.LoadCommitted(samples.Project.ChunkCount(),
		expectedDeps(graph), bake.ChunkParamVersions(samples.Project, graph))
	if err != nil {
		t.Fatal(err)
	}
	if resetReason != "" {
		t.Fatalf("unexpected reset: %s", resetReason)
	}
	if !reflect.DeepEqual(sequence, []int{0, 1, 2, 3}) || len(loaded) != 4 {
		t.Fatalf("loaded sequence=%v count=%d", sequence, len(loaded))
	}
	if err := bake.CompareResults(first.Results, loaded); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptContentResetsWholeSnapshot(t *testing.T) {
	samples := storeSamples(t)
	path := t.TempDir() + "/checkpoint.db"
	fp := BuildFingerprint(samples.Project)
	graph := bake.BuildGraph(samples.Project)
	db, err := Open(path, fp)
	if err != nil {
		t.Fatal(err)
	}
	report := commitAll(t, db, samples.Project, graph)
	if len(report.Results) != 4 {
		t.Fatalf("committed %d chunks", len(report.Results))
	}
	if _, err := db.db.Exec(`UPDATE chunks SET weights=x'00' WHERE chunk_id=1`); err != nil {
		t.Fatal(err)
	}
	loaded, sequence, reason, err := db.LoadCommitted(samples.Project.ChunkCount(),
		expectedDeps(graph), bake.ChunkParamVersions(samples.Project, graph))
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" || len(loaded) != 0 || len(sequence) != 0 {
		t.Fatalf("snapshot was not reset: reason=%q loaded=%d seq=%v", reason, len(loaded), sequence)
	}
	var count int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale chunks remain: %d", count)
	}
}

func TestVersionMismatchResetsWholeSnapshot(t *testing.T) {
	samples := storeSamples(t)
	path := t.TempDir() + "/checkpoint.db"
	fp := BuildFingerprint(samples.Project)
	graph := bake.BuildGraph(samples.Project)
	db, err := Open(path, fp)
	if err != nil {
		t.Fatal(err)
	}
	commitAll(t, db, samples.Project, graph)
	if _, err := db.db.Exec(`UPDATE chunks SET algorithm_version='old-algo' WHERE chunk_id=2`); err != nil {
		t.Fatal(err)
	}
	loaded, _, reason, err := db.LoadCommitted(samples.Project.ChunkCount(),
		expectedDeps(graph), bake.ChunkParamVersions(samples.Project, graph))
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" || len(loaded) != 0 {
		t.Fatalf("version mismatch did not reset: %q", reason)
	}
}

func TestIncrementalCheckpointReuseAfterEdit(t *testing.T) {
	samples := storeSamples(t)
	path := t.TempDir() + "/checkpoint.db"
	fp := BuildFingerprint(samples.Project)
	graph := bake.BuildGraph(samples.Project)
	db, err := Open(path, fp)
	if err != nil {
		t.Fatal(err)
	}
	commitAll(t, db, samples.Project, graph)
	edited := samples.Project.Clone()
	edited.Layers[2].Curve.Points[1].Y += 12_345
	before := cloneLayer(samples.Project.Layers[2])
	after := cloneLayer(edited.Layers[2])
	invalidations := graph.Propagate([]bake.Edit{{
		LayerID: "rock",
		Reason:  "rock curve edited",
		Before:  before,
		After:   after,
	}})
	affected := bake.InvalidationChunks(invalidations)
	if len(affected) == 0 {
		t.Fatal("edit affected no chunks")
	}
	if err := db.InvalidateChunks(affected); err != nil {
		t.Fatal(err)
	}
	editedGraph := bake.BuildGraph(edited)
	loaded, _, reason, err := db.LoadCommitted(edited.ChunkCount(),
		expectedDeps(editedGraph), bake.ChunkParamVersions(edited, editedGraph))
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Fatalf("unexpected reset: %s", reason)
	}
	if len(loaded) != edited.ChunkCount()-len(affected) {
		t.Fatalf("reused %d chunks, want %d", len(loaded), edited.ChunkCount()-len(affected))
	}
	committer := NewCommitter(db)
	engine, err := bake.NewEngine(edited, editedGraph,
		bake.EngineConfig{MaxWorkers: 2, BufferBudgetPx: 24, StopAfter: -1}, committer)
	if err != nil {
		t.Fatal(err)
	}
	incremental, err := engine.Run(context.Background(), affected, loaded)
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := bake.FullBake(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := bake.CompareResults(full, incremental.Results); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path, fp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted, sequence, reason, err := db.LoadCommitted(edited.ChunkCount(),
		expectedDeps(editedGraph), bake.ChunkParamVersions(edited, editedGraph))
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Fatalf("restart reset snapshot: %s", reason)
	}
	if len(restarted) != edited.ChunkCount() || len(sequence) != edited.ChunkCount() {
		t.Fatalf("restart loaded %d chunks seq=%v", len(restarted), sequence)
	}
	if err := bake.CompareResults(full, restarted); err != nil {
		t.Fatal(err)
	}
}

func storeSamples(t *testing.T) *gen.SampleSet {
	t.Helper()
	samples, err := gen.Generate(gen.Options{Seed: 424242, ChunkCols: 2, ChunkRows: 2, ChunkSizePx: 8})
	if err != nil {
		t.Fatal(err)
	}
	return samples
}

func commitAll(t *testing.T, db *Store, project *model.Project, graph *bake.Graph) *bake.RunReport {
	t.Helper()
	engine, err := bake.NewEngine(project, graph,
		bake.EngineConfig{MaxWorkers: 4, BufferBudgetPx: 32, StopAfter: -1},
		NewCommitter(db))
	if err != nil {
		t.Fatal(err)
	}
	report, err := engine.Run(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func expectedDeps(graph *bake.Graph) map[int][]string {
	out := make(map[int][]string)
	for id := 0; id < graph.ChunkCount(); id++ {
		out[id] = graph.ChunkDependencies(id)
	}
	return out
}

func cloneLayer(layer model.Layer) *model.Layer {
	out := layer
	if layer.Curve != nil {
		curve := *layer.Curve
		curve.Points = append([]model.CurvePoint(nil), layer.Curve.Points...)
		out.Curve = &curve
	}
	if layer.Noise != nil {
		noise := *layer.Noise
		out.Noise = &noise
	}
	return &out
}
