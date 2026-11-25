package workbench

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"ballistic/internal/kernel"
	"ballistic/internal/snapshot"
)

func TestEndpointEditIncrementalMatchesFullAndReusesTiles(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, "endpoint.db")
	wb, err := New(kernel.DefaultSeed, store, "frame-endpoint")
	if err != nil {
		t.Fatal(err)
	}
	if err := wb.RunFull(ctx); err != nil {
		t.Fatal(err)
	}

	newA := kernel.Vec2{X: 38, Y: 318}
	plan, err := wb.EditEndpointWorld("remote_plate", &newA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) == 0 || plan.Changes[0].Component != "endpoints" {
		t.Fatalf("expected endpoint version change, got %+v", plan.Changes)
	}
	t.Logf("direct=%v downstream=%v changed=%v attrs=%v", plan.DirectTiles, plan.DownstreamTiles, plan.ChangedHashTiles, plan.AttributionIDs)
	if !contains(plan.InvalidTiles, "tx00_ty03") && !contains(plan.InvalidTiles, "tx01_ty03") {
		t.Fatalf("remote endpoint tiles not invalidated: %+v", plan.InvalidTiles)
	}
	if len(plan.AttributionIDs) != 0 {
		t.Fatalf("unrelated spatial overlap produced attribution invalidation: %v", plan.AttributionIDs)
	}
	if !contains(plan.ReusedTiles, "tx14_ty08") {
		t.Fatalf("unrelated tile not marked reusable: %+v", plan.ReusedTiles)
	}

	if err := wb.RunIncremental(ctx, stringSet(plan.InvalidTiles)); err != nil {
		t.Fatal(err)
	}
	full, err := wb.FreshFullRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmp := CompareRuns(full, wb)
	if !cmp.PixelEqual || !cmp.HashEqual || !cmp.TileMetricsEqual || !cmp.HitStatsEqual {
		t.Fatalf("incremental/full mismatch: %+v", cmp)
	}
	if len(wb.ReusedTileIDs) == 0 {
		t.Fatal("expected persisted unaffected tiles to be reused")
	}
}

func TestRestitutionEditPropagatesToAttributionsAndMatchesFull(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, "restitution.db")
	wb, err := New(kernel.DefaultSeed, store, "frame-restitution")
	if err != nil {
		t.Fatal(err)
	}
	if err := wb.RunFull(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := wb.EditRestitution("floor_main", 6400)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(plan.AttributionIDs, "req_vertical") || !contains(plan.AttributionIDs, "req_diagonal") {
		t.Fatalf("downstream attributions not invalidated: %+v", plan.AttributionIDs)
	}
	if len(plan.DownstreamTiles) == 0 {
		t.Fatal("expected downstream tiles after restitution change")
	}
	if err := wb.RunIncremental(ctx, stringSet(plan.InvalidTiles)); err != nil {
		t.Fatal(err)
	}
	full, err := wb.FreshFullRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmp := CompareRuns(full, wb)
	if !cmp.PixelEqual || !cmp.HashEqual || !cmp.TileMetricsEqual || !cmp.HitStatsEqual {
		t.Fatalf("restitution incremental/full mismatch: %+v", cmp)
	}
}

func TestForcedStopReopensCommittedSnapshotsAndResumes(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "crash.db")
	store, err := snapshot.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := New(kernel.DefaultSeed, store, "frame-crash")
	if err != nil {
		t.Fatal(err)
	}
	committed, stopErr := wb.RunFullThenStopAfter(ctx, 5)
	if !errors.Is(stopErr, ErrForcedStop) || committed != 5 {
		t.Fatalf("stop boundary committed=%d err=%v", committed, stopErr)
	}
	logs, err := store.CommitLog(ctx, wb.FrameID)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 5 || logs[0].TileID != wb.Mapping.TileOrder[0] || logs[4].TileID != wb.Mapping.TileOrder[4] {
		t.Fatalf("commit order unstable: %+v", logs)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := snapshot.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	resume, err := New(kernel.DefaultSeed, reopened, "frame-crash")
	if err != nil {
		t.Fatal(err)
	}
	if err := resume.RunResume(ctx); err != nil {
		t.Fatal(err)
	}
	if len(resume.ReusedTileIDs) != 5 {
		t.Fatalf("reused %d tiles want 5: %v", len(resume.ReusedTileIDs), resume.ReusedTileIDs)
	}
	full, err := resume.FreshFullRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmp := CompareRuns(full, resume)
	if !cmp.PixelEqual || !cmp.HashEqual || !cmp.TileMetricsEqual || !cmp.HitStatsEqual {
		t.Fatalf("resume/full mismatch: %+v", cmp)
	}
}

func TestStaleContentHashIsInvalidatedAndRecomputed(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, "stale.db")
	wb, err := New(kernel.DefaultSeed, store, "frame-stale")
	if err != nil {
		t.Fatal(err)
	}
	if err := wb.RunFull(ctx); err != nil {
		t.Fatal(err)
	}
	tileID := wb.Mapping.TileOrder[10]
	if err := store.CorruptContentHash(ctx, wb.FrameID, tileID); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadValidTile(ctx, wb.Key(tileID), wb.Camera)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != nil {
		t.Fatal("stale tile was not invalidated")
	}
	if err := wb.RunResume(ctx); err != nil {
		t.Fatal(err)
	}
	if !contains(wb.RenderedTileIDs, tileID) {
		t.Fatalf("stale tile not recomputed: rendered=%v", wb.RenderedTileIDs)
	}
}

func openTestStore(t *testing.T, name string) *snapshot.Store {
	t.Helper()
	store, err := snapshot.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func stringSet(items []string) map[string]bool {
	result := map[string]bool{}
	for _, item := range items {
		result[item] = true
	}
	return result
}
