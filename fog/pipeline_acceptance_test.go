package fog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func testStore(t *testing.T, name string) *CheckpointStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func canonicalSequence(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func TestThreePathsPixelInvalidationAndCommitSequence(t *testing.T) {
	ctx := context.Background()
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	expectedBlocks := append([]int(nil), s.Truth.DensityInvalidation.ExpectedBlocks...)
	graph, err := BuildDependencyGraph(s, "c0")
	if err != nil {
		t.Fatal(err)
	}
	manifest := graph.InvalidateVoxel(s.DeclaredEdit)
	if !reflect.DeepEqual(manifest.Blocks, expectedBlocks) {
		t.Fatalf("graph invalidation %v != generator oracle %v", manifest.Blocks, expectedBlocks)
	}
	if len(manifest.Blocks) == 0 || len(manifest.Blocks) == len(graph.Viewport.Tiles) {
		t.Fatalf("expected precise non-full invalidation, got %v", manifest.Blocks)
	}

	incStore := testStore(t, "incremental.db")
	incBaseline := &Pipeline{Scenario: s, Store: incStore, CameraID: "c0"}
	if _, err := incBaseline.RunCheckpointed(ctx, nil); err != nil {
		t.Fatalf("incremental baseline: %v", err)
	}

	resumePath := filepath.Join(t.TempDir(), "resume.db")
	resumeStore, err := OpenCheckpointStore(resumePath)
	if err != nil {
		t.Fatal(err)
	}
	resumeBaseline := &Pipeline{Scenario: s, Store: resumeStore, CameraID: "c0"}
	if _, err := resumeBaseline.RunCheckpointed(ctx, nil); err != nil {
		t.Fatalf("resume baseline: %v", err)
	}

	_, changed, err := ApplyDensityEdit(s, s.DeclaredEdit)
	if err != nil || !changed {
		t.Fatalf("apply edit changed=%v err=%v", changed, err)
	}

	fullStore := testStore(t, "full.db")
	fullPath := &Pipeline{Scenario: s, Store: fullStore, CameraID: "c0"}
	fullRes, err := fullPath.RunCheckpointed(ctx, &manifest)
	if err != nil {
		t.Fatalf("full recompute: %v", err)
	}

	incPath := &Pipeline{Scenario: s, Store: incStore, CameraID: "c0"}
	incRes, err := incPath.RunCheckpointed(ctx, &manifest)
	if err != nil {
		t.Fatalf("incremental after edit: %v", err)
	}
	if !reflect.DeepEqual(incRes.Computed, expectedBlocks) {
		t.Fatalf("incremental computed blocks %v != invalidated %v", incRes.Computed, expectedBlocks)
	}
	if len(incRes.CacheHits) != len(graph.Viewport.Tiles)-len(expectedBlocks) {
		t.Fatalf("cache hits=%d want %d; computed=%v", len(incRes.CacheHits), len(graph.Viewport.Tiles)-len(expectedBlocks), incRes.Computed)
	}

	crash := &Pipeline{Scenario: s, Store: resumeStore, CameraID: "c0", CrashAfterCommits: 7}
	crashRes, crashErr := crash.RunCheckpointed(ctx, &manifest)
	if !errors.Is(crashErr, ErrSimulatedCrash) {
		t.Fatalf("expected simulated crash, got %v", crashErr)
	}
	if crashRes == nil || len(crashRes.CommitSequence) != 7 {
		t.Fatalf("crash prefix sequence=%v", crashRes)
	}
	if err := resumeStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCheckpointStore(resumePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	resumePathPipeline := &Pipeline{Scenario: s, Store: reopened, CameraID: "c0"}
	resumeRes, err := resumePathPipeline.RunCheckpointed(ctx, &manifest)
	if err != nil {
		t.Fatalf("resume render: %v", err)
	}
	if len(resumeRes.Recovered) != 7 {
		t.Fatalf("recovered=%v want first seven blocks", resumeRes.Recovered)
	}

	wantSeq := canonicalSequence(len(graph.Viewport.Tiles))
	for name, res := range map[string]*PipelineResult{"full": fullRes, "incremental": incRes, "resume": resumeRes} {
		if !reflect.DeepEqual(res.CommitSequence, wantSeq) {
			t.Fatalf("%s commit sequence %v != %v", name, res.CommitSequence, wantSeq)
		}
		if err := CompareFrames(fullRes.Frame, res.Frame, 0, 0); err != nil {
			t.Fatalf("%s pixel mismatch: %v", name, err)
		}
		if !reflect.DeepEqual(res.Manifest.Blocks, expectedBlocks) {
			t.Fatalf("%s manifest %v", name, res.Manifest.Blocks)
		}
	}

	rev := TargetRevisionID(s, "c0")
	if _, err := fullStore.db.Exec(`UPDATE block_checkpoints SET content_hash='tampered' WHERE revision_id=? AND block_id=0`, rev); err != nil {
		t.Fatal(err)
	}
	_, err = fullStore.ValidateRecovery(rev, s)
	var fogErr *Error
	if !errors.As(err, &fogErr) || fogErr.Code != EStore || fogErr.BlockID == nil || *fogErr.BlockID != 0 {
		t.Fatalf("expected EStore hash mismatch at block 0, got %v", err)
	}
}

func TestPreviewAndFullConsistency(t *testing.T) {
	ctx := context.Background()
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := RenderFrame(ctx, s, "c0", ModePreview, nil)
	if err != nil {
		t.Fatal(err)
	}
	full, err := RenderFrame(ctx, s, "c0", ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ComparePreviewAndFull(preview, full, s.Recipe); err != nil {
		t.Fatalf("preview/full divergence: %v", err)
	}
	if !preview.Quality.WithinDelayLimit {
		t.Fatalf("preview exceeded delay limit: %.2fms > %dms", preview.Quality.ElapsedMS, s.Recipe.PreviewDelayLimitMS)
	}
	if preview.Quality.StepBudgetPerSpan >= full.Quality.StepBudgetPerSpan {
		t.Fatalf("preview did not reduce steps: %d >= %d", preview.Quality.StepBudgetPerSpan, full.Quality.StepBudgetPerSpan)
	}
	if preview.Quality.ShadowSamplesPerStep >= full.Quality.ShadowSamplesPerStep {
		t.Fatalf("preview did not reduce shadows: %d >= %d", preview.Quality.ShadowSamplesPerStep, full.Quality.ShadowSamplesPerStep)
	}
}

func TestStepErrorMonotonicOnTruthPixels(t *testing.T) {
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	budgets := []int{1, 2, 4, 8, 16, 32, 64}
	for _, pt := range s.Truth.Pixels {
		cam, _ := s.Camera(pt.CameraID)
		ray := cam.RayForPixel(pt.PixelX, pt.PixelY, s.Width, s.Height, pt.BlockID)
		tr := TraceRay(ray, s.Media, s.FixedScale)
		var lastT, lastL float64 = 1e9, 1e9
		for _, steps := range budgets {
			integ := NewIntegrator(s, ModeFull)
			integ.Steps = steps
			got, err := integ.Integrate(tr)
			if err != nil {
				t.Fatal(err)
			}
			errT := absFloat(got.Transmittance - pt.ExpectedTransmittance)
			errL := absFloat(got.Radiance - pt.ExpectedRadiance)
			if errT > lastT+1e-12 || errL > lastL+1e-12 {
				t.Fatalf("pixel %d,%d steps=%d errors T=%.3e/%.3e L=%.3e/%.3e", pt.PixelX, pt.PixelY, steps, errT, lastT, errL, lastL)
			}
			lastT, lastL = errT, errL
		}
	}
}

func TestExceptionChains(t *testing.T) {
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}

	badCamera, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	badCamera.Cameras.Cameras[1].Time = badCamera.Cameras.Cameras[0].Time - 1
	err = ValidateScenario(badCamera)
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != EOrder || fe.Field != "cameras[1].time" {
		t.Fatalf("expected E_ORDER camera field, got %v", err)
	}
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("order error should be schema class")
	}

	badBounds, _ := Generate(DefaultSeed)
	badBounds.Media[0].Bounds.Min.X = badBounds.Media[0].Bounds.Max.X + 1
	if err := ValidateScenario(badBounds); !errors.As(err, &fe) || fe.Code != ESchema || fe.MediaID == "" {
		t.Fatalf("expected E_SCHEMA media bounds, got %v", err)
	}

	_, _, err = ApplyDensityEdit(s, DensityEdit{MediumID: s.Media[0].ID, X: 999, Y: 0, Z: 0, NewValue: 1})
	if !errors.As(err, &fe) || fe.Code != EDens || fe.MediaID != s.Media[0].ID {
		t.Fatalf("expected E_DENS, got %v", err)
	}

	payload, err := EncodeMedium(s.Media[1])
	if err != nil {
		t.Fatal(err)
	}
	corrupt := FlipPayloadByte(payload, len(payload)/2)
	_, err = DecodeMedium(corrupt)
	if !errors.As(err, &fe) || fe.Code != EDecode || fe.MediaID != s.Media[1].ID {
		t.Fatalf("expected E_DECODE with media id, got %v", err)
	}

	err = checkTransmittance(1.25, "r", "span-2")
	if !errors.As(err, &fe) || fe.Code != ETrans || fe.Segment != "span-2" {
		t.Fatalf("expected E_TRANS, got %v", err)
	}

	frameA := &Frame{Width: 2, Height: 1, T: []float64{1, 1}, Radiance: []float64{0, 0}, Versions: FrameVersions{CameraVersion: "a"}}
	frameB := &Frame{Width: 2, Height: 1, T: []float64{1, 0.5}, Radiance: []float64{0, 0}, Versions: FrameVersions{CameraVersion: "b"}}
	err = CompareFrames(frameA, frameB, 0.01, 0.01)
	if !errors.As(err, &fe) || fe.Code != EDiverge || fe.X != 1 || fe.Y != 0 {
		t.Fatalf("expected E_DIVERGE at x=1, got %v", err)
	}
	if fe.Detail == nil {
		t.Fatal("divergence detail missing versions and measured values")
	}
}

func TestBlockFailureRetryIsolation(t *testing.T) {
	ctx := context.Background()
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	store := testStore(t, "faults.db")
	faults := NewFaultController()
	faults.FailBlockPersistent(3, "injected internal integration failure")
	p := &Pipeline{Scenario: s, Store: store, CameraID: "c0", Faults: faults}
	res, err := p.RunCheckpointed(ctx, nil)
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != EBlock || fe.BlockID == nil || *fe.BlockID != 3 {
		t.Fatalf("expected E_BLOCK block 3, got %v", err)
	}
	if len(res.Frame.FailedBlocks) != 1 || res.Frame.FailedBlocks[0] != 3 {
		t.Fatalf("failed blocks=%v", res.Frame.FailedBlocks)
	}
	seq, _ := store.CommitSequence(TargetRevisionID(s, "c0"))
	for _, id := range seq {
		if id == 3 {
			t.Fatal("failed block must not be committed")
		}
	}

	faults.Clear(3)
	retry, err := p.RunCheckpointed(ctx, nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(retry.Recovered) == 0 || !containsInt(retry.Committed, 3) {
		t.Fatalf("retry recovered=%v committed=%v", retry.Recovered, retry.Committed)
	}
	if err := CompareFrames(retry.Frame, mustFullFrame(t, s), 1e-12, 1e-12); err != nil {
		t.Fatalf("retry frame mismatch: %v", err)
	}
}

func mustFullFrame(t *testing.T, s *Scenario) *Frame {
	t.Helper()
	f, err := RenderFrame(context.Background(), s, "c0", ModeFull, nil)
	if err != nil {
		t.Fatal(fmt.Sprintf("full frame: %v", err))
	}
	return f
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
