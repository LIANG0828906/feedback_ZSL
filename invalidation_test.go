package silhouette

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEditInvalidatesOnlyAffectedTilesAndReusesPersistedResults(t *testing.T) {
	ctx := context.Background()
	scene := GenerateScene(DefaultSeed)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "edit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := RunConfig{Workers: 3, VerticesPerWorker: DefaultVerticesPerWorker}
	base, err := RunRealtime(ctx, scene, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	seg, _ := scene.Segment("seg-0")
	point := seg.Controls[1]
	point.Y += 40 * FixedScale
	edited, err := RunEdit(ctx, scene, base.Assembly, Edit{
		Kind: EditSegmentControl, SegmentID: "seg-0", Control: 1, Point: point,
	}, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(edited.Invalidation.InvalidatedTiles) == 0 || len(edited.ReusedTiles) == 0 {
		t.Fatalf("expected partial invalidation, got invalidated=%v reused=%v", edited.Invalidation.InvalidatedTiles, edited.ReusedTiles)
	}
	if len(edited.Commits) != len(edited.Invalidation.InvalidatedTiles) {
		t.Fatalf("commits=%d invalidated=%d", len(edited.Commits), len(edited.Invalidation.InvalidatedTiles))
	}
	checkpoints, err := store.ListCheckpoints(ctx, scene.ID)
	if err != nil {
		t.Fatal(err)
	}
	cpByID := make(map[string]Checkpoint)
	for _, cp := range checkpoints {
		cpByID[cp.TileID] = cp
	}
	for _, id := range edited.ReusedTiles {
		cp, ok := cpByID[id]
		if !ok {
			t.Fatalf("missing reused checkpoint %s", id)
		}
		for _, tile := range edited.Assembly.Tiles {
			if tile.TileID == id {
				if cp.ContentHash != tile.ContentHash || cp.CacheKey != tile.CacheKey {
					t.Fatalf("reused tile %s differs from persisted checkpoint", id)
				}
			}
		}
	}
	allTiles := make(map[string]bool)
	for _, id := range edited.Invalidation.InvalidatedTiles {
		allTiles[id] = true
	}
	for _, id := range edited.ReusedTiles {
		if allTiles[id] {
			t.Fatalf("tile %s both invalidated and reused", id)
		}
		allTiles[id] = true
	}
	if len(allTiles) != len(scene.Tiles) {
		t.Fatalf("invalidation partition covers %d/%d tiles", len(allTiles), len(scene.Tiles))
	}
}

func TestToleranceEditInvalidatesAllTiles(t *testing.T) {
	ctx := context.Background()
	scene := GenerateScene(DefaultSeed)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "tol.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := RunConfig{Workers: 2, VerticesPerWorker: DefaultVerticesPerWorker}
	base, err := RunRealtime(ctx, scene, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := RunEdit(ctx, scene, base.Assembly, Edit{Kind: EditTolerance, Tolerance: 45000}, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(edited.ReusedTiles) != 0 {
		t.Fatalf("tolerance edit reused tiles: %v", edited.ReusedTiles)
	}
	if len(edited.Invalidation.InvalidatedTiles) != len(scene.Tiles) {
		t.Fatalf("invalidated=%d want %d", len(edited.Invalidation.InvalidatedTiles), len(scene.Tiles))
	}
}

func TestDependencyGraphHasExpectedEdges(t *testing.T) {
	scene := GenerateScene(DefaultSeed)
	sampling, err := SampleScene(scene, SamplingOptions{Tolerance: DefaultTolerance, Budget: 100000})
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := Assemble(scene, sampling)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := BuildDependencyGraph(scene, assembly)
	if err != nil {
		t.Fatal(err)
	}
	seg := graph.Nodes["segment:seg-0"]
	if seg.Kind != "curve_segment" || !containsString(seg.DependsOn, "parameter:tolerance") || !containsString(seg.DependsOn, "control:seg-0:1") {
		t.Fatalf("unexpected segment dependencies: %+v", seg)
	}
	invalidated := graph.InvalidateFrom([]string{"control:seg-0:1"})
	if !containsString(invalidated, "polyline:seg-0") || !containsString(invalidated, "segment:seg-0") {
		t.Fatalf("dependency propagation missed downstream nodes: %v", invalidated)
	}
	if containsString(invalidated, "segment:seg-1") {
		t.Fatalf("unrelated segment invalidated: %v", invalidated)
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
