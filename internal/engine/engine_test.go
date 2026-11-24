package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"cadlab/internal/deps"
	"cadlab/internal/model"
	"cadlab/internal/render"
	"cadlab/internal/sample"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	opts := DefaultOptions(t.TempDir())
	opts.DBPath = filepath.Join(t.TempDir(), "checkpoint.sqlite")
	opts.Width = 320
	opts.Height = 200
	opts.TileCols = 6
	opts.TileRows = 4
	opts.ExactDensity = 12
	opts.PreviewDensity = 4
	opts.MaxParallel = 2
	opts.PerTaskMeshBytes = 2 << 20
	opts.TotalMeshBytes = 5 << 20
	return opts
}

func TestPreviewExactIncrementalAndResume(t *testing.T) {
	ds := sample.Generate(sample.DefaultSeed)
	opts := testOptions(t)
	e, err := New(ds, opts)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := e.RenderPreview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if preview.Duration > 50_000_000 {
		t.Fatalf("preview exceeded 50ms: %v", preview.Duration)
	}
	exact, err := e.RenderExact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if preview.Version != exact.Version {
		t.Fatalf("path versions differ: preview=%d exact=%d", preview.Version, exact.Version)
	}
	if preview.Camera != exact.Camera || preview.Clip != exact.Clip {
		t.Fatal("preview and exact paths do not share camera/clip metadata")
	}
	if exact.Rendered != opts.TileCols*opts.TileRows {
		t.Fatalf("full render missed tiles: %d", exact.Rendered)
	}
	order := e.store.WriteOrder()
	if len(order) != opts.TileCols*opts.TileRows {
		t.Fatalf("checkpoint write count %d", len(order))
	}
	for i := range order {
		if order[i] != i {
			t.Fatalf("checkpoint write order unstable at %d: %v", i, order)
		}
	}

	mat := e.state.Patches[1].Material
	mat.BaseColor = model.Color{R: 225, G: 64, B: 48}
	mat.Specular = 0.8
	out, err := e.ApplyMaterial(context.Background(), 2, mat)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Invalidation.InvalidatedTiles) == 0 || len(out.Invalidation.ReusedTiles) == 0 {
		t.Fatalf("material edit did not produce a partial invalidation: %d/%d", len(out.Invalidation.InvalidatedTiles), len(out.Invalidation.ReusedTiles))
	}
	if out.Frame.CacheHits != len(out.Invalidation.ReusedTiles) || out.Frame.Rendered != len(out.Invalidation.InvalidatedTiles) {
		t.Fatalf("cache accounting mismatch: hits=%d rendered=%d report=%d/%d",
			out.Frame.CacheHits, out.Frame.Rendered, len(out.Invalidation.ReusedTiles), len(out.Invalidation.InvalidatedTiles))
	}
	if d := render.CompareFrames(out.Frame, out.Reference); d != nil {
		t.Fatalf("incremental != full: %v", d)
	}
	if out.Comparison == nil || !out.Comparison.FullVsIncremental.Equal ||
		!out.Comparison.CoverageEqual || !out.Comparison.ShadingEqual {
		t.Fatalf("three-way comparison failed: %+v", out.Comparison)
	}
	if out.Frame.Version != 2 {
		t.Fatalf("expected version 2, got %d", out.Frame.Version)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := New(ds, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if e2.state.Version != 2 {
		t.Fatalf("restart did not restore edited scene version: %d", e2.state.Version)
	}
	resumed, err := e2.Resume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Diagnostic != nil || resumed.Frame == nil || resumed.LoadedTiles != opts.TileCols*opts.TileRows {
		t.Fatalf("resume failed: %+v", resumed)
	}
	full, err := e2.RenderExact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d := render.CompareFrames(resumed.Frame, full); d != nil {
		t.Fatalf("resume != full: %v", d)
	}
	cmp, err := e2.ComparePaths()
	if err != nil {
		t.Fatal(err)
	}
	if cmp.FullVsResume == nil || !cmp.FullVsResume.Equal {
		t.Fatalf("full vs resume comparison failed: %+v", cmp)
	}
}

func TestSnapshotMismatchAndIncompleteRecovery(t *testing.T) {
	ds := sample.Generate(sample.DefaultSeed)
	opts := testOptions(t)
	e, err := New(ds, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RenderExact(context.Background()); err != nil {
		t.Fatal(err)
	}
	meta := e.stateMeta(render.PathExact)
	sceneJSON, _ := json.Marshal(e.state)
	if err := e.store.Begin(meta, sceneJSON); err != nil {
		t.Fatal(err)
	}
	first := e.cache[0]
	if err := e.store.CommitTile(meta, first, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := New(ds, opts)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e2.Resume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Diagnostic == nil || out.Diagnostic.Code != model.CodeRecover {
		t.Fatalf("expected E_RECOVER, got %+v", out)
	}
	e2.Close()

	e3, err := New(ds, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e3.Close()
	if _, err := e3.RenderExact(context.Background()); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", e3.opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tile_checkpoints SET content_hash='tampered' WHERE tile_id=2`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e3.Close()

	e4, err := New(ds, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e4.Close()
	out, err = e4.Resume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.Diagnostic == nil || out.Diagnostic.Code != model.CodeSnap {
		t.Fatalf("expected E_SNAP, got %+v", out)
	}
	if len(out.Diagnostic.ConflictingTiles) != 1 || out.Diagnostic.ConflictingTiles[0] != 2 {
		t.Fatalf("E_SNAP did not identify conflicting tile: %+v", out.Diagnostic)
	}
	if out.Frame == nil || !out.FullRerendered {
		t.Fatalf("snapshot mismatch did not force a full rerender: %+v", out)
	}
}

func TestProfileAndAnomalyChannels(t *testing.T) {
	ds := sample.Generate(sample.DefaultSeed)
	opts := testOptions(t)
	e, err := New(ds, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.RenderExact(context.Background()); err != nil {
		t.Fatal(err)
	}
	profile, err := e.Profile(context.Background(), model.Vec3{Y: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Locked || profile.Version != e.state.Version || len(profile.Segments) == 0 {
		t.Fatalf("profile not frozen or empty: %+v", profile)
	}
	seg := profile.Segments[0]
	if seg.A.Label != "A" || seg.B.Label != "B" || !seg.A.World.IsFinite() || !seg.B.World.IsFinite() {
		t.Fatalf("profile endpoint labels/readings invalid: %+v", seg)
	}
	cases := []struct {
		kind string
		code string
	}{
		{AnomalyMissingControl, model.CodeGeom},
		{AnomalyKnotOrder, model.CodeKnot},
		{AnomalyDensity, model.CodePara},
		{AnomalyStage, model.CodeStage},
	}
	for _, tc := range cases {
		res, err := e.InjectAnomaly(context.Background(), tc.kind, 3)
		if err != nil {
			t.Fatalf("%s injection failed: %v", tc.kind, err)
		}
		if res.Diagnostic == nil || res.Diagnostic.Code != tc.code || res.Diagnostic.PatchID < 0 {
			t.Fatalf("%s expected %s with patch, got %+v", tc.kind, tc.code, res)
		}
	}
}

func TestDiffVersionAndScopeDiagnostics(t *testing.T) {
	ds := sample.Generate(sample.DefaultSeed)
	opts := testOptions(t)
	opts.Width, opts.Height, opts.TileCols, opts.TileRows = 160, 120, 4, 3
	e, err := New(ds, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	full, err := e.RenderExact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := e.RenderPreview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d := render.CompareFrames(full, preview); d == nil || d.Code != model.CodeVersion && d.Code != model.CodeDiff {
		// Different LODs are expected to differ in pixels but never in version.
		t.Fatalf("expected pixel difference for different LODs, got %v", d)
	}
	lodScene := *e.preview
	lodScene.Version = full.Version + 1
	other, err := render.RenderFull(context.Background(), &lodScene, render.PathPreview)
	if err != nil {
		t.Fatal(err)
	}
	if d := render.CompareFrames(preview, other); d == nil || d.Code != model.CodeVersion {
		t.Fatalf("expected E_VER, got %v", d)
	}

	broken := *full
	broken.Image = cloneImage(full.Image)
	broken.Image.Pix[0] ^= 0xff
	if d := render.CompareFrames(full, &broken); d == nil || d.Code != model.CodeDiff ||
		d.FirstPixel == nil || d.Field != "RGBA" || d.TileID != 0 {
		t.Fatalf("expected located E_DIFF tile 0, got %+v", d)
	}
	rep := deps.Report{InvalidatedTiles: []int{1, 2}}
	if d := deps.AuditScope(rep, []int{2, 3}); d == nil || d.Code != model.CodeScope ||
		len(d.MislabeledTiles) != 2 || d.MislabeledTiles[0] != 1 || d.MislabeledTiles[1] != 3 {
		t.Fatalf("expected E_SCOPE tiles [1,3], got %+v", d)
	}
}

func TestControlAndDensityEditsStayWithinCoveredTiles(t *testing.T) {
	ds := sample.Generate(sample.DefaultSeed)
	opts := testOptions(t)
	opts.Width, opts.Height, opts.TileCols, opts.TileRows = 240, 150, 5, 3
	e, err := New(ds, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.RenderExact(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertEditScope := func(label string, patchID int, out *EditOutcome) {
		t.Helper()
		if len(out.Invalidation.InvalidatedTiles) == 0 || len(out.Invalidation.ReusedTiles) == 0 {
			t.Fatalf("%s did not reuse unaffected tiles", label)
		}
		covered := map[int]bool{}
		for _, id := range render.PatchCoverage(e.exact, patchID, nil) {
			covered[id] = true
		}
		for _, id := range out.Invalidation.InvalidatedTiles {
			if !covered[id] {
				t.Fatalf("%s invalidated tile %d outside patch %d coverage", label, id, patchID)
			}
		}
		if d := render.CompareFrames(out.Frame, out.Reference); d != nil {
			t.Fatalf("%s incremental mismatch: %v", label, d)
		}
	}

	cp := e.state.Patches[0].Source.Control[0]
	cp.Position.Y += 0.25
	out, err := e.ApplyControlPoint(context.Background(), 1, cp.Row, cp.Col, cp.Position)
	if err != nil {
		t.Fatal(err)
	}
	assertEditScope("control", 1, out)

	out, err = e.ApplyDensity(context.Background(), 3, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertEditScope("density", 3, out)
}

func cloneImage(src *image.NRGBA) *image.NRGBA {
	dst := image.NewNRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}
