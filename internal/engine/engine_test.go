package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"volumediag/internal/diag"
	"volumediag/internal/model"
	"volumediag/internal/volume"
)

func loadSample(t *testing.T) *model.Bundle {
	t.Helper()
	bundle, err := model.LoadBundle(filepath.Join("..", "..", "artifacts", "sample"))
	if err != nil {
		t.Fatalf("load sample: %v", err)
	}
	return bundle
}

func TestFullRenderMatchesAnalyticTruth(t *testing.T) {
	bundle := loadSample(t)
	params := volume.DefaultParams()
	eng, err := Open(bundle, params, "")
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	report, err := eng.Render(context.Background(), ModeFull, RenderOptions{})
	if err != nil {
		t.Fatalf("full render: %v", err)
	}
	if !report.Complete {
		t.Fatalf("full render incomplete: %+v", report)
	}
	truthReport, err := eng.VerifyTruth(context.Background())
	if err != nil {
		t.Fatalf("truth verification: %v\nreport=%+v", err, truthReport)
	}
	if !truthReport.Equal {
		t.Fatalf("truth report not equal: %+v", truthReport)
	}
	t.Logf("truth check pixels=%d rays=%d max_pixel_error=%g max_scatter_error=%g",
		truthReport.PixelsCompared, truthReport.RaysCompared,
		truthReport.MaxPixelError, truthReport.MaxScatterError)
}

func TestIncrementalLightMediumAndGeometryMatchFull(t *testing.T) {
	bundle := loadSample(t)
	params := volume.DefaultParams()
	params.MaxWorkers = 3
	eng, err := Open(bundle, params, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	report, err := eng.Render(context.Background(), ModeFull, RenderOptions{})
	if err != nil || !report.Complete {
		t.Fatalf("full render: err=%v report=%+v", err, report)
	}
	before := eng.Cache()
	intensity := model.C3(6.0, 4.4, 3.1)
	edit, err := eng.ApplyEdit(EditRequest{LightID: "point_key", Intensity: &intensity})
	if err != nil {
		t.Fatalf("light edit: %v", err)
	}
	if edit.AffectedBlocks == 0 || edit.AffectedBlocks >= 48 {
		t.Fatalf("expected a proper subset of blocks to be invalidated, got %+v", edit)
	}
	if edit.ReusedBlocks != 48-edit.AffectedBlocks {
		t.Fatalf("unexpected reuse accounting: %+v", edit)
	}
	inc, err := eng.Render(context.Background(), ModeIncremental, RenderOptions{})
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if !inc.Complete || inc.ComputedBlocks != edit.AffectedBlocks {
		t.Fatalf("incremental report: %+v", inc)
	}
	after := eng.Cache()
	unchanged := 0
	for key, old := range before {
		if _, affected := affectedXY(edit.Invalidated)[[2]int{key.X, key.Y}]; !affected {
			if after[key].Digest != old.Digest {
				t.Fatalf("unaffected block %+v changed", key)
			}
			unchanged++
		}
	}
	if unchanged != 48-edit.AffectedBlocks {
		t.Fatalf("cache reuse mismatch: unchanged=%d expected=%d", unchanged, 48-edit.AffectedBlocks)
	}
	cmp, err := eng.VerifyAgainstFull(context.Background())
	if err != nil {
		t.Fatalf("compare after light edit: %v report=%+v", err, cmp)
	}
	if !cmp.Equal || cmp.PixelsCompared == 0 {
		t.Fatalf("compare report: %+v", cmp)
	}

	scatter := model.C3(0.085, 0.075, 0.065)
	mediumEdit, err := eng.ApplyEdit(EditRequest{Medium: &MediumEdit{Scattering: &scatter}})
	if err != nil {
		t.Fatalf("medium edit: %v", err)
	}
	if mediumEdit.AffectedBlocks != 48 {
		t.Fatalf("medium edit should invalidate all blocks: %+v", mediumEdit)
	}
	if _, err := eng.Render(context.Background(), ModeIncremental, RenderOptions{}); err != nil {
		t.Fatalf("medium incremental: %v", err)
	}
	if cmp, err := eng.VerifyAgainstFull(context.Background()); err != nil || !cmp.Equal {
		t.Fatalf("compare after medium edit: err=%v report=%+v", err, cmp)
	}

	depths := [][]float64{
		{0.00, 0.00, -0.08, 0.00},
		{0.02, 0.08, 0.00, -0.04},
		{0.00, 0.00, 0.03, 0.01},
	}
	geometryEdit, err := eng.ApplyEdit(EditRequest{OccluderID: "occluder_back", Depths: depths})
	if err != nil {
		t.Fatalf("geometry edit: %v", err)
	}
	if geometryEdit.AffectedBlocks == 0 {
		t.Fatal("geometry edit did not invalidate any block")
	}
	if _, err := eng.Render(context.Background(), ModeIncremental, RenderOptions{}); err != nil {
		t.Fatalf("geometry incremental: %v", err)
	}
	if cmp, err := eng.VerifyAgainstFull(context.Background()); err != nil || !cmp.Equal {
		t.Fatalf("compare after geometry edit: err=%v report=%+v", err, cmp)
	}
}

func affectedXY(in []Invalidation) map[[2]int]struct{} {
	out := make(map[[2]int]struct{})
	for _, item := range in {
		if item.Stage == "volume" {
			out[[2]int{item.BlockX, item.BlockY}] = struct{}{}
		}
	}
	return out
}

func TestCheckpointPauseResumeAndInvalidation(t *testing.T) {
	bundle := loadSample(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "checkpoint.db")
	params := volume.DefaultParams()
	params.BatchSize = 4
	params.MaxWorkers = 2
	eng, err := Open(bundle, params, db)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	report, err := eng.Render(context.Background(), ModeResume, RenderOptions{PauseAfterBatches: 2})
	if err != nil {
		t.Fatalf("paused render: %v", err)
	}
	if !report.Paused || report.Complete {
		t.Fatalf("expected paused partial render: %+v", report)
	}
	pausedCache := eng.Cache()
	if len(pausedCache) == 0 || len(pausedCache) == 48 {
		t.Fatalf("unexpected partial cache size %d", len(pausedCache))
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	eng2, err := Open(bundle, params, db)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	state := eng2.State()
	if state.Restored.Invalidated {
		t.Fatalf("valid checkpoint was invalidated: %+v", state)
	}
	if len(eng2.Cache()) != len(pausedCache) {
		t.Fatalf("restored cache size %d != paused %d", len(eng2.Cache()), len(pausedCache))
	}
	resumed, err := eng2.Render(context.Background(), ModeResume, RenderOptions{})
	if err != nil || !resumed.Complete {
		t.Fatalf("resume: err=%v report=%+v", err, resumed)
	}
	if resumed.ComputedBlocks+resumed.ReusedBlocks != 48 {
		t.Fatalf("resume accounting: %+v", resumed)
	}
	if cmp, err := eng2.VerifyAgainstFull(context.Background()); err != nil || !cmp.Equal {
		t.Fatalf("compare after resume: err=%v report=%+v", err, cmp)
	}
	if err := eng2.Close(); err != nil {
		t.Fatalf("close2: %v", err)
	}

	changedParams := params
	changedParams.Step = 0.06
	eng3, err := Open(bundle, changedParams, db)
	if err != nil {
		t.Fatalf("open changed params: %v", err)
	}
	if !eng3.State().Restored.Invalidated {
		t.Fatal("parameter mismatch should invalidate the whole snapshot")
	}
	if len(eng3.Cache()) != 0 {
		t.Fatal("parameter mismatch kept stale blocks")
	}
	_ = eng3.Close()
}

func TestCorruptTruthCoverageIsRejected(t *testing.T) {
	bundle := loadSample(t)
	dir := t.TempDir()
	sceneBytes, err := os.ReadFile(filepath.Join("..", "..", "artifacts", "sample", "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	truthBytes, err := os.ReadFile(filepath.Join("..", "..", "artifacts", "sample", "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scene.json"), sceneBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), truthBytes[:len(truthBytes)-2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := model.LoadBundle(dir); err == nil {
		t.Fatal("expected malformed truth to fail")
	}
	_ = bundle
}

func TestSchedulingOrderAndWorkerCountDoNotChangeOutput(t *testing.T) {
	bundle := loadSample(t)
	p1 := volume.DefaultParams()
	p1.MaxWorkers = 1
	p1.BufferSlots = 1
	p1.BatchSize = 3
	e1, err := Open(bundle, p1, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e1.Render(context.Background(), ModeFull, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	p2 := volume.DefaultParams()
	p2.MaxWorkers = 4
	p2.BufferSlots = 1
	p2.BatchSize = 7
	e2, err := Open(bundle, p2, "")
	if err != nil {
		t.Fatal(err)
	}
	report, err := e2.Render(context.Background(), ModeFull, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.BufferWaitCount == 0 {
		t.Fatal("expected a recorded buffer wait with four workers and one slot")
	}
	cmp, err := compareBlockCaches(e1.Cache(), e2.Cache())
	if err != nil {
		t.Fatalf("scheduling changed output: %v report=%+v", err, cmp)
	}
	if !cmp.Equal {
		t.Fatalf("scheduling changed output: %+v", cmp)
	}
}

func TestCoverageIsIndependentOfLightOrder(t *testing.T) {
	bundle := loadSample(t)
	scene := bundle.Scene
	params := volume.DefaultParams()
	covA, err := computeLightCoverage(scene, 0, params)
	if err != nil {
		t.Fatal(err)
	}
	reversed := scene.Clone()
	for i, j := 0, len(reversed.Lights)-1; i < j; i, j = i+1, j-1 {
		reversed.Lights[i], reversed.Lights[j] = reversed.Lights[j], reversed.Lights[i]
	}
	covB, err := computeLightCoverage(reversed, 0, params)
	if err != nil {
		t.Fatal(err)
	}
	for id, a := range covA {
		b := covB[id]
		if len(a) != len(b) {
			t.Fatalf("coverage size differs for %s: %d != %d", id, len(a), len(b))
		}
		for key := range a {
			if _, ok := b[key]; !ok {
				t.Fatalf("coverage set differs for %s at %+v", id, key)
			}
		}
	}
}

func TestDependencyFailureLocatesFirstBlock(t *testing.T) {
	bundle := loadSample(t)
	_, err := computeLightCoverage(bundle.Scene, -1, volume.DefaultParams())
	if diag.CodeOf(err) != diag.EDep {
		t.Fatalf("code=%s err=%v", diag.CodeOf(err), err)
	}
	de := diag.As(err)
	if de == nil || de.BlockX != 0 || de.BlockY != 0 {
		t.Fatalf("dependency failure did not locate first block: %#v", de)
	}
}

func TestDiffLocatorReportsBlockPixelItemAndValues(t *testing.T) {
	bundle := loadSample(t)
	eng, err := Open(bundle, volume.DefaultParams(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Render(context.Background(), ModeFull, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	left := eng.Cache()
	right := make(map[volume.BlockKey]*volume.BlockResult, len(left))
	for k, v := range left {
		right[k] = v
	}
	key := volume.BlockKey{CameraIndex: 0, CameraID: "cam0", X: 2, Y: 3}
	orig := right[key]
	clone := *orig
	clone.Pixels = append([]volume.PixelResult(nil), orig.Pixels...)
	clone.Pixels[9].Volume[1] += 1e-6
	right[key] = &clone
	report, err := compareBlockCaches(left, right)
	if diag.CodeOf(err) != diag.EDiff {
		t.Fatalf("code=%s err=%v", diag.CodeOf(err), err)
	}
	de := diag.As(err)
	if de == nil || de.BlockX != 2 || de.BlockY != 3 || de.Item != "volume_color" ||
		de.PixelX != clone.Pixels[9].X || de.PixelY != clone.Pixels[9].Y || de.Left == "" || de.Right == "" {
		t.Fatalf("bad diff location: %#v report=%+v", de, report)
	}
}

func TestPreviewJitterPathMatchesFreshFullWithSameParams(t *testing.T) {
	bundle := loadSample(t)
	params := volume.PreviewParams()
	params.MaxWorkers = 3
	params.BatchSize = 6
	eng, err := Open(bundle, params, "")
	if err != nil {
		t.Fatal(err)
	}
	report, err := eng.Render(context.Background(), ModePreview, RenderOptions{})
	if err != nil || !report.Complete {
		t.Fatalf("preview render: err=%v report=%+v", err, report)
	}
	if report.Quality.ErrorBoundPerPixel <= 0 || report.Quality.ActualStep <= 0 || report.Quality.JitterSamples != params.JitterSamples {
		t.Fatalf("preview quality not recorded: %+v", report.Quality)
	}
	cmp, err := eng.VerifyAgainstFull(context.Background())
	if err != nil || !cmp.Equal {
		t.Fatalf("preview/full mismatch: err=%v report=%+v", err, cmp)
	}
	truth, err := eng.VerifyTruth(context.Background())
	if err != nil || !truth.Equal {
		t.Fatalf("preview truth mismatch: err=%v report=%+v", err, truth)
	}
}
