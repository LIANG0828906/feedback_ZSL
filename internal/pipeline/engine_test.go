package pipeline_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"volumekernel/internal/checkpoint"
	"volumekernel/internal/generator"
	"volumekernel/internal/kernel"
	"volumekernel/internal/model"
	"volumekernel/internal/pipeline"
	"volumekernel/internal/scene"
)

func exactSnapshot(t *testing.T) model.SceneSnapshot {
	t.Helper()
	s := generator.Generate(generator.DefaultSeed)
	if err := scene.Validate(s); err != nil {
		t.Fatal(err)
	}
	snap, err := model.NewSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestThreeRenderingPathsArePixelIdentical(t *testing.T) {
	snap := exactSnapshot(t)
	engine := &pipeline.Engine{}
	full, err := engine.Render(snap, pipeline.RenderOptions{
		CameraIndex: 2,
		Mode:        "exact",
		MaxParallel: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	rtStore, err := checkpoint.Open(filepath.Join(dir, "realtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer rtStore.Close()
	realtime, err := (&pipeline.Engine{Store: rtStore}).Render(snap, pipeline.RenderOptions{
		CameraIndex:        2,
		Mode:               "exact",
		MaxParallel:        4,
		UsePersistentCache: true,
		UseCheckpoint:      true,
		CommitCheckpoint:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	resumeStore, err := checkpoint.Open(filepath.Join(dir, "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	resumeEngine := &pipeline.Engine{Store: resumeStore}
	partial, err := resumeEngine.Render(snap, pipeline.RenderOptions{
		CameraIndex:      2,
		Mode:             "exact",
		MaxParallel:      2,
		UseCheckpoint:    true,
		CommitCheckpoint: true,
		CommitLimit:      4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !partial.StoppedEarly {
		t.Fatal("expected partial render to stop early")
	}
	if err := resumeStore.Close(); err != nil {
		t.Fatal(err)
	}
	resumeStore, err = checkpoint.Open(filepath.Join(dir, "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer resumeStore.Close()
	resumed, err := (&pipeline.Engine{Store: resumeStore}).Render(snap, pipeline.RenderOptions{
		CameraIndex:      2,
		Mode:             "exact",
		MaxParallel:      3,
		UseCheckpoint:    true,
		Resume:           true,
		CommitCheckpoint: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.CheckpointReused != 4 {
		t.Fatalf("expected 4 checkpointed blocks to be reused, got %d", resumed.CheckpointReused)
	}
	if !pipeline.FramesEqualPixels(full.Frame, realtime.Frame) {
		t.Fatal("full and realtime frames differ")
	}
	if !pipeline.FramesEqualPixels(full.Frame, resumed.Frame) {
		t.Fatal("full and resumed frames differ")
	}
	if len(realtime.Frame.Queue) == 0 {
		t.Fatal("concurrent realtime render should record shadow-budget queue events")
	}
	rows, err := resumeStore.DB().Query(`SELECT seq,block_x,block_y FROM checkpoint_blocks ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantSeq := 0
	for rows.Next() {
		var seq, bx, by int
		if err := rows.Scan(&seq, &bx, &by); err != nil {
			t.Fatal(err)
		}
		if seq != wantSeq {
			t.Fatalf("checkpoint commit sequence %d, want %d", seq, wantSeq)
		}
		wantSeq++
	}
	if wantSeq != len(resumed.Frame.Blocks) {
		t.Fatalf("checkpoint blocks=%d frame blocks=%d", wantSeq, len(resumed.Frame.Blocks))
	}
}

func TestPreviewAndExactConsistency(t *testing.T) {
	snap := exactSnapshot(t)
	engine := &pipeline.Engine{}
	exact, err := engine.Render(snap, pipeline.RenderOptions{CameraIndex: 2, Mode: "exact"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := engine.Render(snap, pipeline.RenderOptions{CameraIndex: 2, Mode: "preview"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := pipeline.CompareFrames(preview.Frame, exact.Frame, snap.Scene.Recipe.PixelTolerance)
	if err != nil {
		t.Fatalf("compare: %v (report=%+v)", err, report)
	}
	t.Logf("preview elapsed=%.3fms max=%g mean=%g over=%g", preview.Frame.ElapsedMS, report.MaxAbsError, report.MeanAbsError, report.OverToleranceRate)
	if !preview.PreviewDeadlineMet {
		t.Fatalf("preview deadline missed: %.3fms > %dms", preview.Frame.ElapsedMS, snap.Scene.Recipe.PreviewLatencyMS)
	}
	zeroReport, err := pipeline.CompareFrames(preview.Frame, exact.Frame, 0)
	if model.CodeOf(err) != model.EDiverge {
		t.Fatalf("zero tolerance should diverge, got %v", err)
	}
	wantX, wantY := -1, -1
	for i := range preview.Frame.Pixels {
		a, b := preview.Frame.Pixels[i], exact.Frame.Pixels[i]
		if a.Transmittance != b.Transmittance || a.ScatteredRadiance != b.ScatteredRadiance {
			wantX, wantY = a.X, a.Y
			break
		}
	}
	if zeroReport.FirstPixelX != wantX || zeroReport.FirstPixelY != wantY {
		t.Fatalf("first divergent pixel=(%d,%d), want (%d,%d)", zeroReport.FirstPixelX, zeroReport.FirstPixelY, wantX, wantY)
	}
}

func TestEditInvalidationReusesUnaffectedPersistentCache(t *testing.T) {
	snap := exactSnapshot(t)
	dir := t.TempDir()
	store, err := checkpoint.Open(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine := &pipeline.Engine{Store: store}
	base, err := engine.Render(snap, pipeline.RenderOptions{
		CameraIndex:        2,
		Mode:               "exact",
		UsePersistentCache: true,
		MaxParallel:        4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if base.CacheMisses != len(base.Frame.Blocks) {
		t.Fatalf("first render misses=%d blocks=%d", base.CacheMisses, len(base.Frame.Blocks))
	}
	graph, err := pipeline.BuildDependencyGraph(snap, 2)
	if err != nil {
		t.Fatal(err)
	}
	edit := pipeline.Edit{
		Kind:     pipeline.EditVoxel,
		MediumID: "media-beta",
		IX:       2,
		IY:       2,
		IZ:       2,
		Reason:   "diagnostic brush stroke",
	}
	inv, err := pipeline.InvalidateForEdit(graph, edit, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.AffectedBlocks) == 0 || len(inv.AffectedBlocks) == len(base.Frame.Blocks) {
		t.Fatalf("expected a strict subset of blocks invalidated, got %d/%d", len(inv.AffectedBlocks), len(base.Frame.Blocks))
	}
	if inv.CacheRowsDeleted != int64(len(inv.AffectedBlocks)) {
		t.Fatalf("deleted cache rows=%d affected=%d", inv.CacheRowsDeleted, len(inv.AffectedBlocks))
	}
	edited := generator.CloneScene(snap.Scene)
	edited = pipeline.ApplyEdit(edited, edit)
	if err := scene.Validate(edited); err != nil {
		t.Fatal(err)
	}
	editedSnap, err := model.NewSnapshot(edited)
	if err != nil {
		t.Fatal(err)
	}
	after, err := engine.Render(editedSnap, pipeline.RenderOptions{
		CameraIndex:        2,
		Mode:               "exact",
		UsePersistentCache: true,
		MaxParallel:        4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.CacheHits != inv.Unaffected {
		t.Fatalf("cache hits=%d want unaffected=%d", after.CacheHits, inv.Unaffected)
	}
	if after.CacheMisses != len(inv.AffectedBlocks) {
		t.Fatalf("cache misses=%d want affected=%d", after.CacheMisses, len(inv.AffectedBlocks))
	}
}

func TestBlockFailureDoesNotPolluteCommittedCheckpoint(t *testing.T) {
	snap := exactSnapshot(t)
	dir := t.TempDir()
	store, err := checkpoint.Open(filepath.Join(dir, "failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = (&pipeline.Engine{Store: store}).Render(snap, pipeline.RenderOptions{
		CameraIndex:      2,
		Mode:             "exact",
		UseCheckpoint:    true,
		CommitCheckpoint: true,
		MaxParallel:      2,
		FailBlock:        &pipeline.BlockCoord{X: 1, Y: 0},
	})
	var ke *model.Error
	if !errors.As(err, &ke) || ke.Code != model.EBlock || ke.BlockX != 1 || ke.BlockY != 0 {
		t.Fatalf("expected E_BLOCK at (1,0), got %v", err)
	}
	if !ke.HasBlock {
		t.Fatal("E_BLOCK must carry a present block location")
	}
	n, err := store.CheckpointCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("checkpoint count=%d, want 1 committed predecessor", n)
	}
}

func TestScatteringEditInvalidationReusesUnaffectedCache(t *testing.T) {
	snap := exactSnapshot(t)
	dir := t.TempDir()
	store, err := checkpoint.Open(filepath.Join(dir, "scatter-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine := &pipeline.Engine{Store: store}
	base, err := engine.Render(snap, pipeline.RenderOptions{
		CameraIndex: 2, Mode: "exact", UsePersistentCache: true, MaxParallel: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := pipeline.BuildDependencyGraph(snap, 2)
	if err != nil {
		t.Fatal(err)
	}
	edit := pipeline.Edit{Kind: pipeline.EditScattering, MediumID: "media-beta", Reason: "scatter slider"}
	inv, err := pipeline.InvalidateForEdit(graph, edit, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.AffectedBlocks) == 0 || len(inv.AffectedBlocks) >= len(base.Frame.Blocks) {
		t.Fatalf("expected strict subset, got %d/%d", len(inv.AffectedBlocks), len(base.Frame.Blocks))
	}
	edited := pipeline.ApplyEdit(snap.Scene, edit)
	if err := scene.Validate(edited); err != nil {
		t.Fatal(err)
	}
	editedSnap, err := model.NewSnapshot(edited)
	if err != nil {
		t.Fatal(err)
	}
	after, err := engine.Render(editedSnap, pipeline.RenderOptions{
		CameraIndex: 2, Mode: "exact", UsePersistentCache: true, MaxParallel: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.CacheHits != inv.Unaffected || after.CacheMisses != len(inv.AffectedBlocks) {
		t.Fatalf("cache reuse mismatch: hits=%d misses=%d inv=%+v", after.CacheHits, after.CacheMisses, inv)
	}
}

func TestDependencyGraphRecordsUpstreamRaysBlocksAndPixels(t *testing.T) {
	snap := exactSnapshot(t)
	graph, err := pipeline.BuildDependencyGraph(snap, 2)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[pipeline.NodeKind]bool{}
	for _, node := range graph.Nodes {
		seen[node.Kind] = true
	}
	for _, kind := range []pipeline.NodeKind{
		pipeline.NodeDensity, pipeline.NodeCoeff, pipeline.NodeCamera,
		pipeline.NodeRecipe, pipeline.NodeRay, pipeline.NodeBlock, pipeline.NodePixel,
	} {
		if !seen[kind] {
			t.Fatalf("dependency graph is missing node kind %s", kind)
		}
	}
	edgeSet := map[string]bool{}
	for _, e := range graph.Edges {
		edgeSet[e.From+"->"+e.To] = true
	}
	wantPrefixes := []string{"voxel:", "coeff:", "camera:", "recipe:"}
	for _, prefix := range wantPrefixes {
		found := false
		for edge := range edgeSet {
			if strings.HasPrefix(edge, prefix) && strings.Contains(edge, "->ray:") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no edge from %s dependency to a ray node", prefix)
		}
	}
	if len(graph.BlockPixels) == 0 {
		t.Fatal("graph has no block-to-pixel downstream edges")
	}
}

func TestStepBudgetErrorIdentifiesRay(t *testing.T) {
	s := generator.Generate(generator.DefaultSeed)
	_, _, err := kernelRenderOne(s)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = kernelRenderOneWithLimit(s, 8)
	var ke *model.Error
	if !errors.As(err, &ke) || ke.Code != model.EBudget || ke.RayID == "" {
		t.Fatalf("expected E_BUDGET with ray id, got %v", err)
	}
}

func kernelRenderOne(s model.Scene) (model.PixelResult, []string, error) {
	return kernelRenderOneWithLimit(s, 0)
}

func kernelRenderOneWithLimit(s model.Scene, limit int) (model.PixelResult, []string, error) {
	return kernel.RenderRay(s, s.CameraTrack.Samples[2], s.Viewport, s.Viewport.Width/2, s.Viewport.Height/2, kernel.Options{
		Steps:         s.Recipe.Steps,
		ShadowSamples: s.Recipe.ShadowSamples,
		StepBudget:    limit,
	})
}
