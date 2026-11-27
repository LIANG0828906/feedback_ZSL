package volumefog

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"volumefog/store"
)

func TestPreviewExactConsistency(t *testing.T) {
	scene := testScene(t)
	renderer := NewRenderer(nil)
	preview, err := renderer.Render(scene, RenderOptions{
		Mode: "preview", Steps: scene.Recipe.PreviewSteps,
		ShadowSamples: scene.Recipe.PreviewShadowSamples,
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !preview.WithinLatency {
		t.Fatalf("preview missed latency cap: %.1fms > %dms", preview.ElapsedMS, preview.LatencyLimitMS)
	}
	exact, err := renderer.Render(scene, RenderOptions{Mode: "exact"})
	if err != nil {
		t.Fatalf("exact: %v", err)
	}
	if err := ComparePreviewExact(preview, exact); err != nil {
		t.Fatalf("preview/exact: %v", err)
	}
	if preview.Stats.StepsUsed >= exact.Stats.StepsUsed {
		t.Fatalf("preview did not reduce step usage: %d >= %d", preview.Stats.StepsUsed, exact.Stats.StepsUsed)
	}
	if preview.Stats.ShadowSamplesUsed >= exact.Stats.ShadowSamplesUsed {
		t.Fatalf("preview did not reduce shadow samples: %d >= %d",
			preview.Stats.ShadowSamplesUsed, exact.Stats.ShadowSamplesUsed)
	}
}

func TestOverlapSegmentationAndQuantizedResidual(t *testing.T) {
	scene := testScene(t)
	overlap := false
	for _, ray := range SampleRays(scene) {
		segments := RaySegments(scene.Media, ray)
		for _, seg := range segments {
			if seg.T1Q <= seg.T0Q {
				t.Fatalf("non-positive segment")
			}
			if seg.ResidualQ < 0 {
				t.Fatalf("negative residual")
			}
			if len(seg.Active) > 1 {
				overlap = true
				if !sortedStrings(seg.Active) {
					t.Fatalf("active media not sorted: %v", seg.Active)
				}
			}
		}
	}
	if !overlap {
		t.Fatal("sample ray did not exercise an overlapping composite segment")
	}
}

func sortedStrings(v []string) bool {
	for i := 1; i < len(v); i++ {
		if v[i-1] >= v[i] {
			return false
		}
	}
	return true
}

func TestRestartVersionMismatchInvalidatesOnlyAffectedChunks(t *testing.T) {
	scene := testScene(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "restart.db")
	wb, err := NewWorkbench(scene, db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wb.Exact(); err != nil {
		t.Fatal(err)
	}
	if err := wb.Close(); err != nil {
		t.Fatal(err)
	}
	changed := scene.Clone()
	m, _ := changed.MediumByID("fog_a")
	m.ScatteringQ += 10_000
	wb2, err := NewWorkbench(changed, db, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer wb2.Close()
	frame, err := wb2.Exact()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Stats.InvalidatedChunks == 0 || frame.Stats.ComputedChunks == 0 {
		t.Fatalf("no content mismatch invalidation: %+v", frame.Stats)
	}
	if frame.Stats.CacheHits == 0 {
		t.Fatalf("unaffected chunks were not reused: %+v", frame.Stats)
	}
	if len(frame.CommittedSequence) != len(frame.Chunks) {
		t.Fatalf("commit sequence incomplete: %v", frame.CommittedSequence)
	}
}

func TestBlockFailureIsolationAndRetry(t *testing.T) {
	scene := testScene(t)
	baseline, err := NewRenderer(nil).Render(scene, RenderOptions{Mode: "baseline"})
	if err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t, filepath.Join(t.TempDir(), "block.db"))
	defer st.Close()
	renderer := NewRenderer(st)
	target := ChunkCoord{X: 2, Y: 1}
	frame, err := renderer.Render(scene, RenderOptions{
		Mode: "blocked", Persist: true,
		FailChunks: map[ChunkCoord]string{target: "injected"},
	})
	if err == nil || ErrorCode(err) != E_BLOCK {
		t.Fatalf("want E_BLOCK, got %v", err)
	}
	if frame == nil || frame.Stats.FailedChunks != 1 || frame.Stats.ComputedChunks != 7 {
		t.Fatalf("failure polluted other chunks: %+v", frame.Stats)
	}
	recs, err := st.ListChunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 7 {
		t.Fatalf("committed %d chunks, want 7", len(recs))
	}
	for _, rec := range recs {
		if rec.ChunkX == target.X && rec.ChunkY == target.Y {
			t.Fatal("failed chunk was committed")
		}
	}
	retry, err := renderer.Render(scene, RenderOptions{Mode: "retry", Persist: true, UseCache: true})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.Stats.CacheHits != 7 || retry.Stats.ComputedChunks != 1 {
		t.Fatalf("retry cache stats: %+v", retry.Stats)
	}
	if err := CompareFrames(baseline, retry); err != nil {
		t.Fatalf("retry frame differs: %v", err)
	}
}

func TestFrameComparisonLocatesFirstMismatch(t *testing.T) {
	scene := testScene(t)
	a, err := NewRenderer(nil).Render(scene, RenderOptions{Mode: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewRenderer(nil).Render(scene, RenderOptions{Mode: "b"})
	if err != nil {
		t.Fatal(err)
	}
	b.Chunks[3].Pixels[5].Color[1] += 0.001
	err = CompareFrames(a, b)
	if err == nil || ErrorCode(err) != E_DIVERGE {
		t.Fatalf("want E_DIVERGE, got %v", err)
	}
	var fe *FogError
	if !errors.As(err, &fe) || fe.Chunk == nil || fe.Chunk.X != b.Chunks[3].ChunkX || fe.Chunk.Y != b.Chunks[3].ChunkY {
		t.Fatalf("mismatch chunk not located: %+v", fe)
	}
}

func TestPreviewExactDivergenceLocation(t *testing.T) {
	scene := testScene(t)
	renderer := NewRenderer(nil)
	preview, err := renderer.Render(scene, RenderOptions{Mode: "preview",
		Steps: scene.Recipe.PreviewSteps, ShadowSamples: scene.Recipe.PreviewShadowSamples})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := renderer.Render(scene, RenderOptions{Mode: "exact"})
	if err != nil {
		t.Fatal(err)
	}
	exact.Chunks[2].CenterColor[0] += 1
	err = ComparePreviewExact(preview, exact)
	if err == nil || ErrorCode(err) != E_DIVERGE {
		t.Fatalf("want E_DIVERGE, got %v", err)
	}
	var fe *FogError
	if !errors.As(err, &fe) || fe.Chunk == nil || fe.Chunk.X != exact.Chunks[2].ChunkX ||
		fe.Actual == nil || fe.Expected == nil {
		t.Fatalf("bad divergence payload: %+v", fe)
	}
}

func TestValidateTruthReportsEIntLocation(t *testing.T) {
	scene := testScene(t)
	mutated := false
	for i := range scene.Truth.Rays {
		if len(scene.Truth.Rays[i].Segments) > 0 {
			scene.Truth.Rays[i].Segments[0].Transmittance += 0.25
			mutated = true
			break
		}
	}
	if !mutated {
		t.Fatal("no truth segment to mutate")
	}
	err := ValidateTruth(scene)
	if err == nil || ErrorCode(err) != E_INT {
		t.Fatalf("want E_INT, got %v", err)
	}
	var fe *FogError
	if !errors.As(err, &fe) || fe.RayID == "" || fe.Deviation == nil || *fe.Deviation <= 0 {
		t.Fatalf("missing ray/deviation: %+v", fe)
	}
}

func TestScatteringEditIncremental(t *testing.T) {
	scene := testScene(t)
	wb, err := NewWorkbench(scene, filepath.Join(t.TempDir(), "scatter.db"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Close()
	if _, err := wb.Exact(); err != nil {
		t.Fatal(err)
	}
	m, _ := scene.MediumByID("fog_a")
	invs, frame, err := wb.EditAndRecomputeScattering("fog_a", m.ScatteringQ+5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) == 0 || frame.Stats.ComputedChunks == 0 {
		t.Fatalf("scattering edit did not invalidate: inv=%d frame=%+v", len(invs), frame.Stats)
	}
	if frame.Stats.CacheHits+frame.Stats.ComputedChunks != len(frame.Chunks) {
		t.Fatalf("all chunks accounted: %+v", frame.Stats)
	}
}

func TestBudgetQueueCompletesAllChunks(t *testing.T) {
	scene := testScene(t)
	full, err := NewRenderer(nil).Render(scene, RenderOptions{Mode: "full"})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	limited, err := NewRenderer(nil).Render(scene, RenderOptions{
		Mode: "realtime", MaxParallel: 2, RayBuffer: 2,
	})
	if err != nil {
		t.Fatalf("limited: %v", err)
	}
	if limited.Budget.WaitCount == 0 || limited.Budget.DelayedChunks == 0 {
		t.Fatalf("expected budget queue records, got %+v", limited.Budget)
	}
	sawShadowWait := false
	for _, wait := range limited.Budget.WaitEvents {
		if strings.Contains(wait.Reason, "shadow sampling budget exhausted") {
			sawShadowWait = true
			break
		}
	}
	if !sawShadowWait {
		t.Fatalf("no shadow-budget exhaustion wait: %+v", limited.Budget.WaitEvents)
	}
	if len(limited.Chunks) != len(full.Chunks) {
		t.Fatalf("delayed chunks were lost")
	}
	for _, ch := range limited.Chunks {
		if ch.RayBufferFlushes != 32 {
			t.Fatalf("chunk %s flushes=%d want 32", ChunkCoord{X: ch.ChunkX, Y: ch.ChunkY}, ch.RayBufferFlushes)
		}
	}
	if err := CompareFrames(full, limited); err != nil {
		t.Fatalf("budget-limited frame differs: %v", err)
	}
}

func TestIncrementalInvalidationAndPersistentCache(t *testing.T) {
	scene := testScene(t)
	db := filepath.Join(t.TempDir(), "incremental.db")
	wb, err := NewWorkbench(scene, db, t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkbench: %v", err)
	}
	defer wb.Close()
	first, err := wb.Exact()
	if err != nil {
		t.Fatalf("first exact: %v", err)
	}
	var target DependencyRef
	owner := ChunkCoord{}
	found := false
	for _, ch := range first.Chunks {
		for _, d := range ch.Dependencies {
			if d.Kind == "density" {
				target, found = d, true
				owner = ChunkCoord{X: ch.ChunkX, Y: ch.ChunkY}
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("chunk has no density dependency")
	}
	m, _ := scene.MediumByID(target.MediaID)
	x, y, z, ok := m.DensityXYZ(target.Voxel)
	if !ok {
		t.Fatal("bad voxel index")
	}
	old := m.Density[target.Voxel]
	newValue := (old + 61) % (MaxDensity + 1)
	invs, frame, err := wb.EditAndRecomputeVoxel(target.MediaID, x, y, z, newValue)
	if err != nil {
		t.Fatalf("incremental edit: %v", err)
	}
	if len(invs) == 0 {
		t.Fatal("expected invalidations")
	}
	affected := map[ChunkCoord]bool{}
	for _, inv := range invs {
		affected[inv.Chunk] = true
		if inv.MediaID != target.MediaID || inv.Reason == "" {
			t.Fatalf("bad invalidation: %+v", inv)
		}
	}
	if !affected[owner] {
		t.Fatal("chunk containing edited voxel was not invalidated")
	}
	if frame.Stats.CacheHits != len(first.Chunks)-len(affected) {
		t.Fatalf("cache hits=%d want %d", frame.Stats.CacheHits, len(first.Chunks)-len(affected))
	}
	if frame.Stats.ComputedChunks != len(affected) {
		t.Fatalf("recomputed=%d want %d", frame.Stats.ComputedChunks, len(affected))
	}
}

func TestThreeRenderPathsAndCommitSequences(t *testing.T) {
	scene := testScene(t)
	wb, err := NewWorkbench(scene, filepath.Join(t.TempDir(), "paths.db"), t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkbench: %v", err)
	}
	defer wb.Close()
	rep := wb.CompareThreePaths()
	if !rep.Passed {
		t.Fatalf("three-path comparison failed: %+v", rep)
	}
	if rep.RealtimeBudget.WaitCount == 0 {
		t.Fatalf("realtime path did not exercise budget queue")
	}
	if len(rep.ResumeCacheEvents) == 0 {
		t.Fatalf("resume path did not report cache events")
	}
}

func TestFaultChains(t *testing.T) {
	scene := testScene(t)
	wb, err := NewWorkbench(scene, filepath.Join(t.TempDir(), "faults.db"), t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkbench: %v", err)
	}
	defer wb.Close()
	cases := []struct {
		kind string
		code Code
	}{
		{"decode", E_DECODE},
		{"order", E_ORDER},
		{"dens", E_DENS},
	}
	for _, tc := range cases {
		rep := wb.DiagnoseFault(tc.kind, 0, 0)
		if rep.Error == nil || rep.Error.Code != tc.code {
			t.Fatalf("%s: got %+v want %s", tc.kind, rep.Error, tc.code)
		}
	}
	rep := wb.DiagnoseFault("block", 0, 0)
	if rep.Error == nil || rep.Error.Code != E_BLOCK || rep.Error.Chunk == nil {
		t.Fatalf("block: %+v", rep)
	}
	if !rep.RetryOK {
		t.Fatalf("block retry failed: %+v", rep)
	}
}

func TestOpenStoreIsSQLiteOrDocumentedFallback(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "backend.db"))
	defer st.Close()
	if st.Backend() == "" {
		t.Fatal("empty backend")
	}
	rec := store.ChunkRecord{ChunkX: 1, ChunkY: 2, CommitSeq: 0, RayDigest: "d", CacheKey: "k",
		ContentHash: "c", RecipeVersion: "r", AlgorithmVersion: "a", CameraVersion: "cam",
		SceneVersion: "s", Width: 8, Height: 8, Transmittance: 0.5, Scattering: 0.25,
		Payload: []byte("payload"), CreatedUnixUS: 1}
	if err := st.PutChunk(rec); err != nil {
		t.Fatalf("PutChunk: %v", err)
	}
	got, err := st.GetChunk(1, 2)
	if err != nil || got.CacheKey != "k" {
		t.Fatalf("GetChunk: %+v %v", got, err)
	}
}

func TestStepBudgetMonotonicityAndErrorBound(t *testing.T) {
	scene := testScene(t)
	rays := SampleRays(scene)
	var probe Ray
	best := -1
	for _, r := range rays {
		if n := len(RaySegments(scene.Media, r)); n > best {
			probe, best = r, n
		}
	}
	probes, err := ProbeStepBudgets(scene, probe, []int{8, 16, 32, 64, 96, 128},
		scene.Recipe.ShadowSamples)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range probes {
		t.Logf("steps=%d T=%.12g rad=%.12g dT=%.3g dL=%.3g bound=%.3g", p.Steps, p.Transmittance, p.Radiance,
			p.TransmittanceDeviation, p.RadianceDeviation, p.ErrorBound)
		if i > 0 {
			if p.Transmittance > probes[i-1].Transmittance+1e-15 {
				t.Fatalf("transmittance increased at steps=%d", p.Steps)
			}
			if p.ErrorBound > probes[i-1].ErrorBound+1e-15 {
				t.Fatalf("error bound did not tighten at steps=%d", p.Steps)
			}
		}
		if p.RadianceDeviation > p.ErrorBound+1e-12 {
			t.Fatalf("steps=%d deviation %g exceeds bound %g", p.Steps, p.RadianceDeviation, p.ErrorBound)
		}
	}
	if probes[0].RadianceDeviation == 0 {
		t.Fatal("coarse probe did not exercise radiance error")
	}
}
