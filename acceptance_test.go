package iconcut

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testBundle(t *testing.T, specIndex int) *Bundle {
	t.Helper()
	dir := t.TempDir()
	specs := DefaultSpecs()
	bundleDir, _, err := GenerateBundle(dir, specs[specIndex])
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, err := LoadBundle(bundleDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return b
}

func errorCode(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e.Code
	}
	return ""
}

func TestGeneratorLoadValidationAndMetaCodes(t *testing.T) {
	root := t.TempDir()
	for _, sp := range DefaultSpecs() {
		dir, _, err := GenerateBundle(root, sp)
		if err != nil {
			t.Fatal(err)
		}
		b, err := LoadBundle(dir)
		if err != nil {
			t.Fatalf("LoadBundle %s: %v", sp.ID, err)
		}
		if b.TruthFingerprint() == "" {
			t.Fatal("missing truth fingerprint")
		}
	}

	b := testBundle(t, 1)
	dir := t.TempDir()
	src := filepath.Join(dir, "source.png")
	if err := os.WriteFile(src, []byte("not a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPNG(src); errorCode(err) != EDecode {
		t.Fatalf("corrupt png code = %v, want E_DECODE (%v)", errorCode(err), err)
	}

	bad := *b.Manifest
	bad.Channels = 4
	p := filepath.Join(dir, "manifest.json")
	writeTestJSON(t, p, bad)
	_, err := LoadManifest(p)
	if errorCode(err) != EMeta || fieldOf(err) != "channels" {
		t.Fatalf("channels mismatch err=%v", err)
	}

	bad = *b.Manifest
	bad.Declared.TileWidth++
	writeTestJSON(t, p, bad)
	_, err = LoadManifest(p)
	if errorCode(err) != EMeta || fieldOf(err) != "declared.tile_width" {
		t.Fatalf("tile divisibility err=%v", err)
	}

	g := b.Manifest.Geometry
	g.Homography[2][2] = 0
	g.Homography[0][0] = 0
	g.Homography[1][1] = 0
	if err := ValidateGeometry(g); errorCode(err) != EGeom || fieldOf(err) != "geometry.homography" {
		t.Fatalf("degenerate homography err=%v", err)
	}
	g = b.Manifest.Geometry
	g.Radial.Radius = -1
	if err := ValidateGeometry(g); errorCode(err) != EGeom || fieldOf(err) != "geometry.radial.radius" {
		t.Fatalf("bad radius err=%v", err)
	}
}

func fieldOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Field
	}
	return ""
}

func writeTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGeometryControlsMonotonicAndReproducible(t *testing.T) {
	b := testBundle(t, 1)
	m := b.Manifest
	var maxes []float64
	for _, order := range []int{0, 1, 2} {
		_, st, err := ControlResiduals(m.Geometry, m.Controls, order)
		if err != nil {
			t.Fatal(err)
		}
		if st.Max > m.Declared.Tolerance.ControlPX {
			t.Fatalf("order %d control residual %v > %v", order, st.Max, m.Declared.Tolerance.ControlPX)
		}
		maxes = append(maxes, st.Max)
	}
	if maxes[0] < maxes[1] || maxes[1] < maxes[2] {
		t.Fatalf("residual stats are not monotonic non-increasing: %v", maxes)
	}
	a, _, err := CorrectWhole(b.Source, m.Geometry, 1)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := CorrectWhole(b.Source, m.Geometry, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Pix) != string(c.Pix) {
		t.Fatal("correction is not pixel-reproducible")
	}
}

func TestTilesExactWholeAndBandError(t *testing.T) {
	b := testBundle(t, 1)
	m := b.Manifest
	mp, err := NewMapper(m.Geometry)
	if err != nil {
		t.Fatal(err)
	}
	whole, _, err := CorrectWhole(b.Source, m.Geometry, m.Declared.InterpOrder)
	if err != nil {
		t.Fatal(err)
	}
	tiles, err := BuildTiles(m.Corrected.W, m.Corrected.H, m.Declared.TileWidth, m.Declared.TileHeight, m.Declared.BandPX, m.Declared.InterpOrder, mp)
	if err != nil {
		t.Fatal(err)
	}
	for _, tile := range tiles {
		img, _, err := CorrectTile(b.Source, mp, tile, m.Declared.InterpOrder, m.Declared.BandPX)
		if err != nil {
			t.Fatalf("tile %d: %v", tile.Index, err)
		}
		for y := 0; y < tile.Core.H; y++ {
			for x := 0; x < tile.Core.W; x++ {
				gi := ((tile.Core.Y+y)*whole.W + tile.Core.X + x) * 3
				li := (y*img.W + x) * 3
				if string(whole.Pix[gi:gi+3]) != string(img.Pix[li:li+3]) {
					t.Fatalf("tile %d pixel %d,%d differs", tile.Index, x, y)
				}
			}
		}
	}

	small, err := BuildTiles(m.Corrected.W, m.Corrected.H, 24, 24, 5, 1, mp)
	if err != nil {
		t.Fatal(err)
	}
	for i, tile := range small {
		if i >= 4 {
			break
		}
		if _, _, err := CorrectTile(b.Source, mp, tile, 1, 5); err != nil {
			t.Fatalf("small tile %d: %v", i, err)
		}
	}

	bad, err := BuildTiles(m.Corrected.W, m.Corrected.H, m.Declared.TileWidth, m.Declared.TileHeight, 0, 1, mp)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = CorrectTile(b.Source, mp, bad[7], 1, 0)
	if errorCode(err) != EBand || fieldOf(err) != "tiles[7].band_px" {
		t.Fatalf("band error = %v", err)
	}
}

func TestParameterSeedGrowEmptyCodes(t *testing.T) {
	b := testBundle(t, 1)
	p := b.Manifest.Declared.Segment
	bad := p
	bad.ColorTolerance = -1
	if err := ValidateSegmentParams(bad); errorCode(err) != EPara || fieldOf(err) != "declared.segment.color_tolerance" {
		t.Fatalf("color param err=%v", err)
	}
	bad = p
	bad.GradientThreshold = 999
	if err := ValidateSegmentParams(bad); errorCode(err) != EPara {
		t.Fatalf("gradient param err=%v", err)
	}
	bad = p
	bad.MorphSize = 4
	if err := ValidateSegmentParams(bad); errorCode(err) != EPara {
		t.Fatalf("morph param err=%v", err)
	}
	if _, _, err := RegionGrow(nil, p, 0); errorCode(err) != EEmpty {
		t.Fatalf("empty err=%v", err)
	}
	corr, _, err := CorrectWhole(b.Source, b.Manifest.Geometry, 1)
	if err != nil {
		t.Fatal(err)
	}
	bad = p
	bad.SeedPoints = []IPoint{{X: 2, Y: 2}}
	if _, _, err := RegionGrow(corr, bad, 0); errorCode(err) != ESeed {
		t.Fatalf("background seed err=%v", err)
	}
}

func TestRegionMismatchListsExtraAndMissing(t *testing.T) {
	truth := []RegionTruth{{ID: 1, Area: 100}, {ID: 2, Area: 200}}
	stats := []RegionStats{{ID: 1, Area: 100}, {ID: 3, Area: 777}}
	e := CompareToTruth(stats, truth, Tolerances{RegionCentroidPX: 1, RegionAreaRel: 0.01})
	if errorCode(e) != ERegion {
		t.Fatalf("code=%v err=%v", errorCode(e), e)
	}
	var ge *Error
	errors.As(e, &ge)
	if ge.Details["extra"] == nil || ge.Details["missing"] == nil {
		t.Fatalf("missing extra/missing details: %+v", ge.Details)
	}
}

func TestStateMachineIllegalTransitionKeepsState(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k := TileKey{TileIndex: 2, TX: 2, TY: 0, Stage: StageGrow}
	if _, err := st.Transition(ctx, "img", k, StateRunning, 0, nil); err != nil {
		t.Fatal(err)
	}
	rec := &CommitRecord{ImageID: "img", TileIndex: 2, TX: 2, TY: 0, Stage: StageGrow, Digest: "d", ContentHash: "h", AlgorithmVersion: AlgorithmVersion, SummaryJSON: "{}", Artifact: []byte{1, 2, 3}}
	if err := st.CommitStage(ctx, rec); err != nil {
		t.Fatal(err)
	}
	_, err = st.Transition(ctx, "img", k, StateRunning, 0.5, nil)
	if errorCode(err) != EState {
		t.Fatalf("illegal restart code=%v err=%v", errorCode(err), err)
	}
	states, err := st.ListTileStates(ctx, "img")
	if err != nil || len(states) != 1 {
		t.Fatalf("states=%v err=%v", states, err)
	}
	if states[0].State != StateCommitted || states[0].Progress != 1 {
		t.Fatalf("state changed after illegal transition: %+v", states[0])
	}
}

func runTestBundle(t *testing.T, b *Bundle, name string, opts ...RunOptions) (*Report, string) {
	t.Helper()
	db := filepath.Join(t.TempDir(), name+".db")
	o := RunOptions{DBPath: db}
	if len(opts) > 0 {
		o = opts[0]
		o.DBPath = db
	}
	rep, err := RunBundle(context.Background(), b, o)
	if err != nil {
		t.Fatalf("RunBundle: %v", err)
	}
	return rep, db
}

func TestPipelinePassesTruthAndThreePathsIdentical(t *testing.T) {
	b := testBundle(t, 1)
	ctx := context.Background()
	full, fullDB := runTestBundle(t, b, "full")

	incrementalManifest := *b.Manifest
	incrementalManifest.Declared.Budget.Workers = 1
	incrementalManifest.Declared.Budget.MaxQueuedTasks = 1
	incBundle := &Bundle{Dir: b.Dir, Manifest: &incrementalManifest, Source: b.Source, Reference: b.Reference}
	inc, incDB := runTestBundle(t, incBundle, "incremental")
	if d := CompareReports(full, inc); d != nil {
		t.Fatalf("incremental differs: %+v", d)
	}

	resumeDB := filepath.Join(t.TempDir(), "resume.db")
	prefix, err := RunBundle(ctx, b, RunOptions{DBPath: resumeDB, StopAfterCommits: 7})
	if err != nil || !prefix.Interrupted {
		t.Fatalf("prefix err=%v interrupted=%v", err, prefix.Interrupted)
	}
	first, err := LoadCommitOrder(ctx, resumeDB, b.Manifest.ImageID)
	if err != nil || len(first) != 7 {
		t.Fatalf("prefix commits=%d err=%v", len(first), err)
	}
	resume, err := RunBundle(ctx, b, RunOptions{DBPath: resumeDB})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if d := CompareReports(full, resume); d != nil {
		t.Fatalf("resume differs: %+v", d)
	}
	fullCommits, err := LoadCommitOrder(ctx, fullDB, b.Manifest.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	resumeCommits, err := LoadCommitOrder(ctx, resumeDB, b.Manifest.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	if d := CompareCommitSequences(fullCommits, resumeCommits); d != nil {
		t.Fatalf("commit sequence mismatch: %+v", d)
	}
	if d, _ := CompareDatabases(ctx, fullDB, incDB, b.Manifest.ImageID); d != nil {
		t.Fatalf("db sequence mismatch: %+v", d)
	}
}

func TestCheckpointCorruptionInvalidatesSnapshot(t *testing.T) {
	b := testBundle(t, 1)
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "corrupt.db")
	if _, err := RunBundle(ctx, b, RunOptions{DBPath: db}); err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.db.Exec(`UPDATE commits SET content_hash='bad' WHERE image_id=? AND tile_index=3 AND stage='clean'`, b.Manifest.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	rep, err := RunBundle(ctx, b, RunOptions{DBPath: db})
	if err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if !rep.SnapshotReset {
		t.Fatal("corrupt checkpoint did not invalidate snapshot")
	}
	if len(rep.CommittedKeys) != 48 {
		t.Fatalf("fresh commit keys = %d, want 48", len(rep.CommittedKeys))
	}
}

func TestFaultRetryProducesSameMask(t *testing.T) {
	b := testBundle(t, 1)
	ctx := context.Background()
	base, _ := runTestBundle(t, b, "base")
	db := filepath.Join(t.TempDir(), "fault.db")
	faults := NewFaultOnce(7, StageClean, EInternal)
	rep, err := RunBundle(ctx, b, RunOptions{DBPath: db, Faults: faults})
	if errorCode(err) != EInternal || len(rep.Failures) == 0 {
		t.Fatalf("expected injected failure, rep=%+v err=%v", rep, err)
	}
	retry, err := RunBundle(ctx, b, RunOptions{DBPath: db})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if d := CompareLabelMasks(base.Mask, retry.Mask, 48, 48); d != nil {
		t.Fatalf("retry mask differs: %+v", d)
	}
	if d := CompareStats(base.Stats, retry.Stats); d != nil {
		t.Fatalf("retry stats differ: %+v", d)
	}
}

func TestSchedulerQueueWaitAndBufferCap(t *testing.T) {
	s := NewScheduler(Budget{Workers: 1, MaxQueuedTasks: 1, MaxThreadPixels: 10})
	release := make(chan struct{})
	jobs := []Job[int]{
		{Tile: Tile{Index: 0}, Stage: StageMapping, Pixels: 1, Fn: func(context.Context) (int, error) { <-release; return 1, nil }},
		{Tile: Tile{Index: 1}, Stage: StageMapping, Pixels: 1, Fn: func(context.Context) (int, error) { return 2, nil }},
		{Tile: Tile{Index: 2}, Stage: StageMapping, Pixels: 1, Fn: func(context.Context) (int, error) { return 3, nil }},
		{Tile: Tile{Index: 3}, Stage: StageMapping, Pixels: 99, Fn: func(context.Context) (int, error) { return 4, nil }},
	}
	done := make(chan []JobResult[int], 1)
	go func() { done <- RunJobs(context.Background(), s, jobs, nil) }()
	time.Sleep(80 * time.Millisecond)
	close(release)
	results := <-done
	if len(results) != len(jobs) {
		t.Fatalf("results=%d jobs=%d", len(results), len(jobs))
	}
	var sawCap bool
	for _, r := range results {
		if r.Err != nil && errorCode(r.Err) == EPara {
			sawCap = true
		}
	}
	if !sawCap {
		t.Fatal("pixel buffer cap did not return E_PARA")
	}
	waits := s.Waits()
	if len(waits) == 0 || waits[0].Reason != "queue_budget_exhausted" || waits[0].WaitNS <= 0 {
		t.Fatalf("missing blocking queue wait: %+v", waits)
	}
}

func TestPreviewWithinToleranceAndDivergenceCode(t *testing.T) {
	b := testBundle(t, 1)
	rep, _ := runTestBundle(t, b, "preview-full", RunOptions{SkipTruthCheck: true})
	preview, err := RunPreview(rep.Corrected, b.Manifest, b.Manifest.Declared.Segment, PreviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.WithinDelayLimit || preview.DelayMS > int64(b.Manifest.Declared.Budget.PreviewDelayMaxMS) {
		t.Fatalf("preview delay %dms > %dms", preview.DelayMS, b.Manifest.Declared.Budget.PreviewDelayMaxMS)
	}
	if preview.ParamVersion != rep.Versions.Param {
		t.Fatal("preview parameter snapshot version differs")
	}
	if e := ComparePreview(rep.Stats, preview.Stats, rep.Mask, preview.Upscaled, b.Manifest.Declared.Tolerance, preview.ParamVersion, rep.Versions.Mapping); e != nil {
		t.Fatalf("default preview diverges: %v details=%+v", e, e.Details)
	}
	bad, err := RunPreview(rep.Corrected, b.Manifest, b.Manifest.Declared.Segment, PreviewOptions{DownsampleStep: 2, IterationBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	e := ComparePreview(rep.Stats, bad.Stats, rep.Mask, bad.Upscaled, b.Manifest.Declared.Tolerance, bad.ParamVersion, rep.Versions.Mapping)
	if errorCode(e) != EDiverge {
		t.Fatalf("forced divergence code=%v err=%v", errorCode(e), e)
	}
	var de *Error
	errors.As(e, &de)
	for _, key := range []string{"region_count_exact", "region_count_preview", "centroid_deviation_px", "area_relative_error", "first_region_id", "first_diff_pixel"} {
		if de.Details[key] == nil {
			t.Fatalf("E_DIVERGE missing %s: %+v", key, de.Details)
		}
	}
}
