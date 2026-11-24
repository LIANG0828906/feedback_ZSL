package pipeline_test

import (
	"context"
	"path/filepath"
	"testing"

	"cadpipe/internal/cad"
	"cadpipe/internal/pipeline"
	"cadpipe/internal/sample"
	"cadpipe/internal/store"
)

func TestIncrementalMatchesFullPixelForPixel(t *testing.T) {
	ctx := context.Background()
	bundle, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoints.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p, err := pipeline.New(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes, pipeline.Options{
		Workers: 2, BufferCount: 2, Store: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	fullReport, err := p.RunFull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(fullReport.Recomputed) != bundle.Camera.TileCount() {
		t.Fatalf("full recomputed %d tiles, want %d", len(fullReport.Recomputed), bundle.Camera.TileCount())
	}

	beforeHash := make(map[int]string)
	beforeColor := make(map[int][]byte)
	for id, tile := range p.Cache {
		beforeHash[id] = tile.ContentHash
		beforeColor[id] = append([]byte(nil), tile.Color...)
	}

	patch := bundle.Scene.Patch("surface-alpha")
	oldCenter := patch.ControlPoints[1][1]
	newCenter := oldCenter
	newCenter.Z += cad.Millimetre(3)
	plan, err := p.UpdateControlPoint("surface-alpha", 1, 1, newCenter)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.TileIDs) == 0 {
		t.Fatal("control edit produced no invalid tiles")
	}
	t.Logf("plan tiles=%v", plan.TileIDs)
	t.Logf("alpha coverage=%v", p.PatchTiles["surface-alpha"])
	t.Logf("tile37 patches=%v", p.TilePatches[37])

	incReport, err := p.RunPlan(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(incReport.Recomputed) != len(plan.TileIDs) {
		t.Fatalf("incremental recomputed %d, planned %d", len(incReport.Recomputed), len(plan.TileIDs))
	}

	invalid := make(map[int]bool)
	for _, id := range plan.TileIDs {
		invalid[id] = true
	}
	unaffected := 0
	for id := 0; id < bundle.Camera.TileCount(); id++ {
		if invalid[id] {
			continue
		}
		unaffected++
		if p.Cache[id].ContentHash != beforeHash[id] {
			t.Fatalf("unaffected tile %d hash changed", id)
		}
		if string(p.Cache[id].Color) != string(beforeColor[id]) {
			t.Fatalf("unaffected tile %d pixels changed", id)
		}
	}
	if unaffected == 0 {
		t.Fatal("test did not leave any unaffected tiles")
	}

	want, err := pipeline.NewWithVersions(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes,
		pipeline.Options{Workers: 1, BufferCount: 1}, p.Versions.Clone())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := want.RunFull(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.CompareWith(want); err != nil {
		t.Fatal(err)
	}
}

func TestRestartRecoversOnlyVersionMatchingSnapshot(t *testing.T) {
	ctx := context.Background()
	bundle, _ := sample.Generate(sample.DefaultSeed)
	dbPath := filepath.Join(t.TempDir(), "restart.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := pipeline.New(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes, pipeline.Options{Workers: 2, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	p.CommitHook = func(_ int, committed int) {
		if committed == 3 {
			panic("simulated kill boundary")
		}
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = p.RunFull(ctx)
	}()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	recovered, err := pipeline.New(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes, pipeline.Options{Store: st2})
	if err != nil {
		t.Fatal(err)
	}
	n, err := recovered.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("recovered %d committed tiles, want 3", n)
	}
	missing, err := recovered.ResumeMissing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Recomputed) != bundle.Camera.TileCount()-3 {
		t.Fatalf("resume recomputed %d, want %d", len(missing.Recomputed), bundle.Camera.TileCount()-3)
	}
}

func TestKnotWeightAndCameraVersions(t *testing.T) {
	ctx := context.Background()
	bundle, _ := sample.Generate(sample.DefaultSeed)
	p, err := pipeline.New(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes, pipeline.Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.RunFull(ctx); err != nil {
		t.Fatal(err)
	}
	before := p.Versions.Patches["surface-alpha"]
	planKnots, err := p.UpdateKnots("surface-alpha", bundle.Scene.Patch("surface-alpha").KnotsU, bundle.Scene.Patch("surface-alpha").KnotsV)
	if err != nil || len(planKnots.TileIDs) == 0 {
		t.Fatalf("knot plan: %v %+v", err, planKnots)
	}
	afterKnots := p.Versions.Patches["surface-alpha"]
	if afterKnots.Knot != before.Knot+1 {
		t.Fatalf("knot version not incremented: %+v", afterKnots)
	}

	weights := cloneWeightsForTest(bundle.Scene.Patch("surface-alpha").Weights)
	weights[1][1] = 2
	planWeights, err := p.UpdateWeights("surface-alpha", weights)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.RunPlan(ctx, planWeights); err != nil {
		t.Fatal(err)
	}
	if p.Versions.Patches["surface-alpha"].Weight != before.Weight+1 {
		t.Fatal("weight version not incremented")
	}

	want, err := pipeline.NewWithVersions(bundle.Scene, bundle.Camera, bundle.Truth.ClipProbes,
		pipeline.Options{Workers: 1}, p.Versions.Clone())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := want.RunFull(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.CompareWith(want); err != nil {
		t.Fatal(err)
	}

	cameraBefore := p.Versions.Camera
	cameraPlan, err := p.UpdateCamera(bundle.Camera)
	if err != nil || len(cameraPlan.TileIDs) == 0 {
		t.Fatalf("camera plan: %v %+v", err, cameraPlan)
	}
	if p.Versions.Camera != cameraBefore+1 {
		t.Fatal("camera version not incremented")
	}
}

func cloneWeightsForTest(in [][]float64) [][]float64 {
	out := make([][]float64, len(in))
	for i := range in {
		out[i] = append([]float64(nil), in[i]...)
	}
	return out
}
