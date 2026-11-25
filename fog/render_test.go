package fog

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func renderExact(t *testing.T, e *Engine, opts RenderOptions) *Frame {
	t.Helper()
	if opts.Mode == "" {
		opts.Mode = RenderExact
	}
	frame, err := e.Render(context.Background(), opts)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	return frame
}

func TestRenderDeterministicAcrossConcurrency(t *testing.T) {
	s := smallScene(t)
	serial := renderExact(t, NewEngine(s, nil), RenderOptions{Mode: RenderExact, MaxParallelTiles: 1})
	parallel := renderExact(t, NewEngine(s, nil), RenderOptions{Mode: RenderExact, MaxParallelTiles: 4})
	if len(serial.Pixels) != s.Width*s.Height || len(serial.Pixels) != len(parallel.Pixels) {
		t.Fatalf("coverage mismatch: %d/%d pixels", len(serial.Pixels), len(parallel.Pixels))
	}
	for i := range serial.Pixels {
		a, b := serial.Pixels[i], parallel.Pixels[i]
		if a.X != b.X || a.Y != b.Y || a.Radiance != b.Radiance || a.Transmittance != b.Transmittance || a.RayDigest != b.RayDigest {
			t.Fatalf("concurrency changed pixel %d: %+v vs %+v", i, a, b)
		}
	}
	if !stringSlicesEqual(serial.CommitSequence, parallel.CommitSequence) {
		t.Fatalf("commit sequence changed: %v vs %v", serial.CommitSequence, parallel.CommitSequence)
	}
	if serial.Stats.MaxParallelObserved > 1 {
		t.Fatalf("serial render observed %d parallel tiles", serial.Stats.MaxParallelObserved)
	}
	if parallel.Stats.MaxParallelObserved > 4 {
		t.Fatalf("parallel cap exceeded: %d", parallel.Stats.MaxParallelObserved)
	}
	if parallel.Stats.RayBufferPeak > s.Recipe.RayBufferPerWorker {
		t.Fatalf("ray buffer cap exceeded: %d > %d", parallel.Stats.RayBufferPeak, s.Recipe.RayBufferPerWorker)
	}
	if parallel.Decision.StepsActual > s.Recipe.Steps {
		t.Fatalf("primary step cap exceeded: %d > %d", parallel.Decision.StepsActual, s.Recipe.Steps)
	}
	if parallel.Decision.ShadowActual > s.Recipe.ShadowSamples {
		t.Fatalf("shadow cap exceeded: %d > %d", parallel.Decision.ShadowActual, s.Recipe.ShadowSamples)
	}
}

func TestPreviewAndExactConsistency(t *testing.T) {
	s := smallScene(t)
	preview := renderExact(t, NewEngine(s, nil), RenderOptions{Mode: RenderPreview})
	exact := renderExact(t, NewEngine(s, nil), RenderOptions{Mode: RenderExact})
	if preview.Decision.StepsConfigured >= exact.Decision.StepsConfigured {
		t.Fatalf("preview did not reduce steps: %d vs %d", preview.Decision.StepsConfigured, exact.Decision.StepsConfigured)
	}
	if preview.Decision.ShadowConfigured >= exact.Decision.ShadowConfigured {
		t.Fatalf("preview did not reduce shadow samples")
	}
	if !preview.Decision.LatencyMet {
		t.Fatalf("preview exceeded declared latency: %.3fms > %dms", preview.Decision.ElapsedMS, preview.Decision.LatencyLimitMS)
	}
	comp, err := CompareFrames(preview, exact, s.Recipe.Tolerance)
	if err != nil {
		t.Fatalf("preview/exact diverged: %v (max=%.9g mean=%.9g)", err, comp.MaxDifference, comp.MeanDifference)
	}
	if !comp.SameCoverage || !comp.SameSnapshotVersion || !comp.SameCameraVersion {
		t.Fatalf("preview/exact metadata mismatch: %+v", comp)
	}
	if preview.MediaSnapshotVersion != exact.MediaSnapshotVersion || preview.CameraVersion != exact.CameraVersion {
		t.Fatalf("snapshot/camera version mismatch")
	}
}

func TestPersistentCacheReuseAndVoxelInvalidation(t *testing.T) {
	s := smallScene(t)
	storePath := filepath.Join(t.TempDir(), "cache.vfogj")
	store, err := OpenCheckpointStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(s, store)
	first := renderExact(t, e, RenderOptions{Mode: RenderExact, MaxParallelTiles: 2})
	if first.Stats.CacheHits != 0 || first.Stats.RenderedTiles != len(first.Tiles) {
		t.Fatalf("first render should be all misses: %+v", first.Stats)
	}
	graph := e.DependencyGraph()
	if graph == nil {
		t.Fatal("dependency graph missing")
	}
	var chosen *DepNode
	var chosenRep *InvalidationReport
	for _, n := range graph.Nodes() {
		if n.Kind != "density" {
			continue
		}
		rep := graph.Invalidate(n.ID)
		if len(rep.Tiles) > 0 && len(rep.Tiles) < len(first.Tiles) {
			node := n
			chosen = &node
			chosenRep = rep
			break
		}
	}
	if chosen == nil {
		t.Fatal("could not find a partially invalidating density voxel")
	}
	mediaID := SourceMediaID(chosen.ID)
	_, m := s.MediaByID(mediaID)
	if m == nil {
		t.Fatalf("cannot parse media from %q", chosen.ID)
	}
	idx := sourceIndex(t, chosen.ID)
	old := m.Density[idx]
	newValue := old + 1
	if newValue > DensityMax {
		newValue = old - 1
	}
	rep, err := e.EditDensity(mediaID, idx, newValue)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Tiles) != len(chosenRep.Tiles) || !stringSlicesEqual(rep.Tiles, chosenRep.Tiles) {
		t.Fatalf("invalidation changed between queries: %v vs %v", rep.Tiles, chosenRep.Tiles)
	}
	second := renderExact(t, e, RenderOptions{Mode: RenderExact, MaxParallelTiles: 2})
	wantHits := len(first.Tiles) - len(rep.Tiles)
	if second.Stats.CacheHits != wantHits {
		t.Fatalf("cache hits=%d want=%d; invalidated=%v; stats=%+v", second.Stats.CacheHits, wantHits, rep.Tiles, second.Stats)
	}
	var rendered []string
	for _, id := range second.Stats.CacheHitTiles {
		_ = id
	}
	rendered = append(rendered, second.Stats.CommittedTiles...)
	sort.Strings(rendered)
	if !stringSlicesEqual(rendered, rep.Tiles) {
		t.Fatalf("recomputed tiles=%v invalidated=%v", rendered, rep.Tiles)
	}
	if len(rep.Items) == 0 || len(rep.Items[0].Sources) == 0 {
		t.Fatalf("invalidation report lacks downstream source detail: %+v", rep)
	}
	third := renderExact(t, e, RenderOptions{Mode: RenderExact, MaxParallelTiles: 2})
	if third.Stats.CacheHits != len(first.Tiles) {
		t.Fatalf("unchanged render should reuse every tile, got %d/%d", third.Stats.CacheHits, len(first.Tiles))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoefficientInvalidationAndCacheReuse(t *testing.T) {
	s := smallScene(t)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "coeff.vfogj"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := NewEngine(s, store)
	first := renderExact(t, e, RenderOptions{Mode: RenderExact})
	graph := e.DependencyGraph()
	var chosen *DepNode
	var rep *InvalidationReport
	for _, n := range graph.Nodes() {
		if n.Kind != "coefficient" {
			continue
		}
		r := graph.Invalidate(n.ID)
		if len(r.Tiles) > 0 && len(r.Tiles) < len(first.Tiles) {
			node := n
			chosen, rep = &node, r
			break
		}
	}
	if chosen == nil {
		t.Skip("sample has no partially visible media coefficient")
	}
	mediaID := SourceMediaID(chosen.ID)
	_, m := s.MediaByID(mediaID)
	oldA, oldS := m.AbsorptionFP, m.ScatteringFP
	if _, err := e.EditCoefficient(mediaID, oldA+1000, oldS+1000); err != nil {
		t.Fatal(err)
	}
	second := renderExact(t, e, RenderOptions{Mode: RenderExact})
	if second.Stats.CacheHits != len(first.Tiles)-len(rep.Tiles) {
		t.Fatalf("coefficient edit cache hits=%d want=%d invalidated=%v",
			second.Stats.CacheHits, len(first.Tiles)-len(rep.Tiles), rep.Tiles)
	}
	if !stringSlicesEqual(rep.Unchanged, second.Stats.CacheHitTiles) {
		t.Fatalf("unchanged tiles=%v cache hits=%v", rep.Unchanged, second.Stats.CacheHitTiles)
	}
}

func TestStructureChangeInvalidatesCachedTiles(t *testing.T) {
	s := smallScene(t)
	store, err := OpenCheckpointStore(filepath.Join(t.TempDir(), "structure.vfogj"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := NewEngine(s, store)
	first := renderExact(t, e, RenderOptions{Mode: RenderExact})
	s.Media = append(s.Media, uniformMedium("new-body", AABB{Min: V(0.2, -0.2, 0.2), Max: V(0.4, 0.4, 0.8)},
		[3]int{2, 2, 2}, 500, 50_000, 50_000, 0, 9))
	if f := s.Validate(); f != nil {
		t.Fatalf("edited scene invalid: %v", f)
	}
	if s.StructureHash() == e.LastFrame().StructureVersion {
		t.Fatal("structure version did not change")
	}
	second := renderExact(t, e, RenderOptions{Mode: RenderExact})
	if second.Stats.CacheHits != 0 {
		t.Fatalf("structure change reused %d stale tiles", second.Stats.CacheHits)
	}
	if second.Stats.RenderedTiles != len(first.Tiles) {
		t.Fatalf("structure change rendered %d/%d tiles", second.Stats.RenderedTiles, len(first.Tiles))
	}
}

func TestShadowBudgetBlocksAndRecordsWait(t *testing.T) {
	b := newShadowBudget(2)
	ctx := context.Background()
	if got, err := b.acquire(ctx, 2, "tile-a", "ray-a"); err != nil || got != 2 {
		t.Fatalf("initial acquire: %d %v", got, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = b.acquire(ctx, 1, "tile-b", "ray-b")
	}()
	select {
	case <-done:
		t.Fatal("second acquire did not block on exhausted budget")
	case <-time.After(15 * time.Millisecond):
	}
	b.release(2)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked acquire did not resume")
	}
	b.release(1)
	stats := b.stats()
	if stats.PeakOccupancy != 2 {
		t.Fatalf("peak occupancy=%d want=2", stats.PeakOccupancy)
	}
	events := b.queueEvents()
	found := false
	for _, e := range events {
		if e.Reason == "shadow-sample-budget" && e.Blocked && e.TileID == "tile-b" && e.WaitMS > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing blocked queue record: %+v", events)
	}
}

func sourceIndex(t *testing.T, source string) int {
	t.Helper()
	i := strings.LastIndexByte(source, ':')
	if i < 0 {
		t.Fatalf("bad source %q", source)
	}
	n, err := strconv.Atoi(source[i+1:])
	if err != nil {
		t.Fatalf("bad source index %q: %v", source, err)
	}
	return n
}

func TestFrameComparisonReportsFirstDivergence(t *testing.T) {
	a := &Frame{Width: 2, Height: 1, MediaSnapshotVersion: "m", CameraVersion: "c",
		Pixels: []PixelValue{{X: 0, Y: 0, Radiance: 1}, {X: 1, Y: 0, Radiance: 2}}}
	b := &Frame{Width: 2, Height: 1, MediaSnapshotVersion: "m", CameraVersion: "c",
		Pixels: []PixelValue{{X: 0, Y: 0, Radiance: 1.01}, {X: 1, Y: 0, Radiance: 2.5}}}
	comp, err := CompareFrames(a, b, 0.1)
	if err == nil {
		t.Fatal("expected divergence")
	}
	f, ok := FaultAs(err)
	if !ok || f.Code != EDiverge || f.Pixel == nil || f.Pixel.X != 1 || f.Pixel.Y != 0 {
		t.Fatalf("wrong divergence fault: %v", err)
	}
	if math.Abs(comp.FirstDifference-0.5) > 1e-12 {
		t.Fatalf("difference=%g", comp.FirstDifference)
	}
}
