package fog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func testSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{MaxParallelTiles: 2, RayBufferPerWorker: 16, ShadowBudgetTotal: 2_000_000}
}

func TestSchedulerBudgetsQueueAndBlockRetry(t *testing.T) {
	sample, _ := GenerateSample()
	scene := &sample.Scene
	tiles := TileGrid(scene.Viewport, scene.Recipe)
	scheduler := NewScheduler(testSchedulerConfig())
	failedTile := TileCoord{X: 1, Y: 1}
	scheduler.Faults.FailNext(failedTile, 1)

	report := scheduler.Run(context.Background(), scene, tiles, IntegrationOptions{})
	if report.Failures[tileKey(failedTile)] == nil {
		t.Fatal("expected injected block failure")
	}
	if fe := report.Failures[tileKey(failedTile)]; fe.Code != CodeBlock || fe.Tile == nil || *fe.Tile != failedTile {
		t.Fatalf("bad block error: %#v", fe)
	}
	if len(report.Tiles) != len(tiles)-1 {
		t.Fatalf("expected %d successful tiles, got %d", len(tiles)-1, len(report.Tiles))
	}
	if report.Resource.MaxActiveTiles > 2 {
		t.Fatalf("parallel tile limit exceeded: %d", report.Resource.MaxActiveTiles)
	}
	if report.Resource.RayBufferHighWater > 16 {
		t.Fatalf("ray buffer limit exceeded: %d", report.Resource.RayBufferHighWater)
	}
	foundChunkRecord := false
	for _, record := range report.Queue {
		if record.Reason == "ray_buffer_chunked" {
			foundChunkRecord = true
		}
	}
	if !foundChunkRecord {
		t.Fatal("expected ray buffer chunk queue detail")
	}

	retry, err := RenderTile(scene, failedTile, TileRenderConfig{Options: IntegrationOptions{}, RayBufferLimit: 16})
	if err != nil {
		t.Fatalf("retry should succeed after one-shot fault: %v", err)
	}
	if retry.Tile != failedTile {
		t.Fatal("retry returned wrong tile")
	}
}

func TestShadowBudgetExhaustionBlocksAndRecordsWait(t *testing.T) {
	sample, _ := GenerateSample()
	scene := &sample.Scene
	scheduler := NewScheduler(SchedulerConfig{MaxParallelTiles: 2, RayBufferPerWorker: 64, ShadowBudgetTotal: 1})
	go func() {
		time.Sleep(80 * time.Millisecond)
		scheduler.GrantShadowBudget(1 << 40)
	}()
	report := scheduler.Run(context.Background(), scene, TileGrid(scene.Viewport, scene.Recipe), IntegrationOptions{})
	if len(report.Failures) != 0 {
		t.Fatalf("expected queued tiles to resume after budget grant: %#v", report.Failures)
	}
	found := false
	for _, record := range report.Queue {
		if record.Reason == "shadow_budget_exhausted" {
			found = true
			if record.WaitMS < 40 {
				t.Fatalf("expected measurable blocking wait, got %dms", record.WaitMS)
			}
		}
	}
	if !found {
		t.Fatal("expected shadow budget queue record")
	}
}

func TestPreciseVoxelInvalidationAndCacheReuse(t *testing.T) {
	sample, _ := GenerateSample()
	scene := &sample.Scene
	graph := BuildDependencyGraph(scene)
	all := TileGrid(scene.Viewport, scene.Recipe)

	var chosenMedium string
	var chosenVoxel [3]int
	var plan *InvalidationReport
	for _, medium := range scene.Media {
		for z := 0; z < medium.Density.Resolution[2] && plan == nil; z++ {
			for y := 0; y < medium.Density.Resolution[1] && plan == nil; y++ {
				for x := 0; x < medium.Density.Resolution[0] && plan == nil; x++ {
					voxel := [3]int{x, y, z}
					candidate, err := PlanVoxelEdit(scene, graph, medium.ID, voxel, 3)
					if err != nil {
						t.Fatal(err)
					}
					if len(candidate.AffectedTiles) > 0 && len(candidate.AffectedTiles) < len(all) {
						plan = candidate
						chosenMedium = medium.ID
						chosenVoxel = voxel
					}
				}
			}
		}
	}
	if plan == nil {
		t.Fatal("could not find a selectively affecting voxel")
	}
	if len(plan.ReusedTiles) == 0 {
		t.Fatal("expected unaffected tiles for precise invalidation")
	}
	affectedCoord := plan.AffectedTiles[0]
	reusedCoord := plan.ReusedTiles[0]
	affectedBefore, err := RenderTile(scene, affectedCoord, TileRenderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	reusedBefore, err := RenderTile(scene, reusedCoord, TileRenderConfig{})
	if err != nil {
		t.Fatal(err)
	}

	applied, err := ApplyVoxelEdit(scene, graph, chosenMedium, chosenVoxel, 9)
	if err != nil {
		t.Fatal(err)
	}
	if applied.MediumID != chosenMedium || !applied.ContainsTile(affectedCoord) {
		t.Fatalf("invalidation report missing medium/tile: %+v", applied)
	}

	affectedAfter, err := RenderTile(scene, affectedCoord, TileRenderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	reusedAfter, err := RenderTile(scene, reusedCoord, TileRenderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if affectedAfter.CacheKey == affectedBefore.CacheKey || affectedAfter.ContentHash == affectedBefore.ContentHash {
		t.Fatal("affected tile cache key/hash did not change")
	}
	if reusedAfter.CacheKey != reusedBefore.CacheKey || reusedAfter.ContentHash != reusedBefore.ContentHash {
		t.Fatalf("unaffected tile cache changed: before=%s after=%s", reusedBefore.CacheKey, reusedAfter.CacheKey)
	}
}

func TestPreciseScatteringInvalidation(t *testing.T) {
	sample, _ := GenerateSample()
	scene := &sample.Scene
	graph := BuildDependencyGraph(scene)
	report, err := ApplyScatteringEdit(scene, graph, "fog-b", FixedFromFloat(0.22))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.AffectedTiles) == 0 {
		t.Fatal("expected scattering edit to affect tiles")
	}
	if len(report.ReusedTiles) == 0 {
		t.Fatal("expected some background tiles to remain unaffected")
	}
}

func TestSQLiteThreeWayPixelAndSequenceConsistency(t *testing.T) {
	sample, _ := GenerateSample()
	dir := t.TempDir()
	report, err := RunThreeWayConsistency(context.Background(), &sample.Scene, dir, testSchedulerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if report.FirstDiff != nil {
		t.Fatalf("three-way mismatch: %+v", report.FirstDiff)
	}
	if !report.Equal || len(report.Realtime.Sequence) != 12 {
		t.Fatalf("unexpected report equality/sequence: equal=%v seq=%d", report.Equal, len(report.Realtime.Sequence))
	}
	if len(report.Resume.StaleCheckpoints) == 0 || len(report.Resume.RecomputedTiles) == 0 {
		t.Fatal("resume path should have detected and recomputed the corrupted checkpoint")
	}
}

func TestRestartHashMismatchInvalidatesTile(t *testing.T) {
	sample, _ := GenerateSample()
	scene := &sample.Scene
	path := t.TempDir() + "/restart.db"
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	tile, err := RenderTile(scene, TileCoord{X: 0, Y: 0}, TileRenderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitTile(context.Background(), *tile); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CorruptCheckpointForTest(path, tile.Tile, "recipe_version", 999); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, stale, err := reopened.LoadValidTile(context.Background(), scene, tile.Tile)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != nil || stale == nil || stale.Field != "recipe_version" {
		t.Fatalf("expected stale recipe_version, loaded=%v stale=%+v", loaded != nil, stale)
	}
	sequence, err := reopened.CheckpointSequence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sequence) != 0 {
		t.Fatalf("stale tile should be deleted, sequence=%v", sequence)
	}
}

func TestCompareTileLocatesE_DIFFField(t *testing.T) {
	sample, _ := GenerateSample()
	tile, err := RenderTile(&sample.Scene, TileCoord{X: 1, Y: 1}, TileRenderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	modified := *tile
	modified.Scattering += 0.5
	err = CompareTileResults(tile.Tile, tile, &modified, 1e-10)
	fe := AsError(err)
	if fe == nil || fe.Code != CodeDiff || fe.Tile == nil || *fe.Tile != tile.Tile || fe.FieldName != "scattering" {
		t.Fatalf("expected E_DIFF scattering tile, got %#v", err)
	}
}

func TestPreviewConsistencyAndInjectedDivergence(t *testing.T) {
	sample, _ := GenerateSample()
	scene := &sample.Scene
	preview, err := RenderPreview(scene)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := RenderExact(scene)
	if err != nil {
		t.Fatal(err)
	}
	EvaluatePreview(scene, preview, exact)
	if preview.Divergence != nil {
		t.Fatalf("default preview should be within declared tolerance: %+v max=%.4f", preview.Divergence, preview.MaxPixelDelta)
	}
	if !preview.WithinDelay {
		t.Fatalf("preview elapsed %dms exceeded %dms", preview.ElapsedMS, preview.DelayLimitMS)
	}

	preview.Frame.Pixels[0].Radiance[0] += 1
	delta, divergence := ComparePreviewExact(scene, preview.Frame, exact)
	if divergence == nil || divergence.Code != CodeDiverge || divergence.Pixel == nil {
		t.Fatalf("expected E_DIVERGE with pixel, delta=%v err=%#v", delta, divergence)
	}
	if divergence.Differences[0] <= 0 {
		t.Fatalf("expected three measured channel differences: %+v", divergence.Differences)
	}
}

func TestCheckpointPreciseReuseAfterVoxelEdit(t *testing.T) {
	sample, _ := GenerateSample()
	scene := &sample.Scene
	graph := BuildDependencyGraph(scene)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "precise.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	all := TileGrid(scene.Viewport, scene.Recipe)
	for _, coord := range all {
		tile, err := RenderTile(scene, coord, TileRenderConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CommitTile(context.Background(), *tile); err != nil {
			t.Fatal(err)
		}
	}

	var report *InvalidationReport
	var voxel [3]int
	for _, medium := range scene.Media {
		for z := 0; z < medium.Density.Resolution[2] && report == nil; z++ {
			for y := 0; y < medium.Density.Resolution[1] && report == nil; y++ {
				for x := 0; x < medium.Density.Resolution[0] && report == nil; x++ {
					candidateVoxel := [3]int{x, y, z}
					candidate, err := PlanVoxelEdit(scene, graph, medium.ID, candidateVoxel, 5)
					if err != nil {
						t.Fatal(err)
					}
					if len(candidate.AffectedTiles) > 0 && len(candidate.ReusedTiles) > 0 {
						report = candidate
						voxel = candidateVoxel
					}
				}
			}
		}
	}
	if report == nil {
		t.Fatal("no selective voxel found")
	}
	if _, err := ApplyVoxelEdit(scene, graph, report.MediumID, voxel, 8); err != nil {
		t.Fatal(err)
	}

	reused := report.ReusedTiles[0]
	loaded, stale, err := store.LoadValidTile(context.Background(), scene, reused)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || stale != nil {
		t.Fatalf("unaffected tile should persist and load, loaded=%v stale=%+v", loaded != nil, stale)
	}

	affected := report.AffectedTiles[0]
	loadedAffected, staleAffected, err := store.LoadValidTile(context.Background(), scene, affected)
	if err != nil {
		t.Fatal(err)
	}
	if loadedAffected != nil || staleAffected == nil || staleAffected.Field != "content_hash" {
		t.Fatalf("affected tile should be invalidated by content hash, loaded=%v stale=%+v", loadedAffected != nil, staleAffected)
	}
}
