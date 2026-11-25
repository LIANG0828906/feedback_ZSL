package engine_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"uvenhance/internal/apperr"
	"uvenhance/internal/cache"
	"uvenhance/internal/engine"
	"uvenhance/internal/samplegen"
	"uvenhance/internal/yuv"
)

func testSample(t *testing.T) *yuv.Sample {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sample")
	if _, err := samplegen.Generate(context.Background(), dir,
		samplegen.Config{ID: "engine_test", Width: 128, Height: 96}, 7721); err != nil {
		t.Fatal(err)
	}
	s, err := yuv.LoadSample(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func openStore(t *testing.T) (*cache.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	st, err := cache.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

func opts() engine.Options {
	return engine.Options{
		ParamVersion: 1, AlignmentVersion: 1, Workers: 2,
		BufferBytesPerWorker: 32 << 20,
	}
}

func TestIncrementalInvalidationOnlyAffectedStages(t *testing.T) {
	s := testSample(t)
	store, _ := openStore(t)
	eng := engine.New(store)
	ctx := context.Background()
	p := yuv.DefaultParams()
	first, err := eng.RunLive(ctx, s, p, opts())
	if err != nil {
		t.Fatal(err)
	}
	if first.Stats.RecomputeCount != 8 || first.Stats.CacheHitCount != 0 {
		t.Fatalf("unexpected cold stats: %+v", first.Stats)
	}
	for _, spec := range cache.Stages {
		out := first.Stages[spec.ID]
		if spec.Kind == cache.KindPlane {
			if out.BitDepth != s.Metadata.BitDepth {
				t.Fatalf("stage %s changed bit depth", spec.ID)
			}
			if spec.Plane == "Y" && (out.Width != s.Y.Width || out.Height != s.Y.Height) {
				t.Fatalf("stage %s changed luma resolution", spec.ID)
			}
			if spec.Plane != "Y" && (out.Width != s.U.Width || out.Height != s.U.Height) {
				t.Fatalf("stage %s changed chroma resolution", spec.ID)
			}
		}
	}
	yHash := first.Stages[cache.StageYContrast].OutputHash
	vHash := first.Stages[cache.StageVGain].OutputHash

	p2 := p
	p2.ChromaU.Gain = 1.17
	second, err := eng.RunLive(ctx, s, p2, opts())
	if err != nil {
		t.Fatal(err)
	}
	if second.Stats.RecomputeCount != 2 || second.Stats.CacheHitCount != 6 {
		t.Fatalf("expected U.gain + interleave only, stats=%+v invalidations=%+v",
			second.Stats, second.Invalidations)
	}
	assertInvalid(t, second, cache.StageUGain, "parameter_changed")
	assertInvalid(t, second, cache.StageInterleave, "upstream_changed")
	if second.Stages[cache.StageYContrast].OutputHash != yHash {
		t.Fatal("unaffected luma output was not reused")
	}
	if second.Stages[cache.StageVGain].OutputHash != vHash {
		t.Fatal("unaffected chroma V output was not reused")
	}

	p3 := p2
	p3.Luma.SharpenAmount = 0.9
	third, err := eng.RunLive(ctx, s, p3, opts())
	if err != nil {
		t.Fatal(err)
	}
	if third.Stats.RecomputeCount != 3 || third.Stats.CacheHitCount != 5 {
		t.Fatalf("expected sharpen, local contrast, interleave; stats=%+v", third.Stats)
	}
	assertInvalid(t, third, cache.StageYSharpen, "parameter_changed")
	assertInvalid(t, third, cache.StageYContrast, "upstream_changed")
	assertInvalid(t, third, cache.StageInterleave, "upstream_changed")
}

func TestInputPlaneChangeInvalidatesOnlyThatChainAndInterleave(t *testing.T) {
	s := testSample(t)
	store, _ := openStore(t)
	eng := engine.New(store)
	ctx := context.Background()
	p := yuv.DefaultParams()
	if _, err := eng.RunLive(ctx, s, p, opts()); err != nil {
		t.Fatal(err)
	}
	s.U.Data[0] = (s.U.Data[0] + 7) & 0xff
	res, err := eng.RunLive(ctx, s, p, opts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.RecomputeCount != 3 || res.Stats.CacheHitCount != 5 {
		t.Fatalf("expected U smooth/U gain/interleave, stats=%+v invalidations=%+v", res.Stats, res.Invalidations)
	}
	assertInvalid(t, res, cache.StageUSmooth, "input_changed")
	assertInvalid(t, res, cache.StageUGain, "upstream_changed")
	assertInvalid(t, res, cache.StageInterleave, "upstream_changed")
}

func TestRestartVersionMismatchInvalidatesWholeSnapshot(t *testing.T) {
	s := testSample(t)
	store, _ := openStore(t)
	p := yuv.DefaultParams()
	if _, err := engine.New(store).Run(context.Background(), s, p, opts()); err != nil {
		t.Fatal(err)
	}
	bumped := opts()
	bumped.ParamVersion = 2
	res, err := engine.New(store).Run(context.Background(), s, p, bumped)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stats.SnapshotRejected || res.Stats.RecomputeCount != 8 || res.Stats.CacheHitCount != 0 {
		t.Fatalf("version mismatch did not invalidate whole snapshot: %+v", res.Stats)
	}
}

func TestRestartInputHashMismatchInvalidatesWholeSnapshot(t *testing.T) {
	source := testSample(t)
	store, _ := openStore(t)
	if _, err := engine.New(store).Run(context.Background(), source, yuv.DefaultParams(), opts()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "other")
	if _, err := samplegen.Generate(context.Background(), dir,
		samplegen.Config{ID: "different_input", Width: 128, Height: 96}, 9999); err != nil {
		t.Fatal(err)
	}
	other, err := yuv.LoadSample(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.New(store).Run(context.Background(), other, yuv.DefaultParams(), opts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stats.SnapshotRejected || res.Stats.CacheHitCount != 0 || res.Stats.RecomputeCount != 8 {
		t.Fatalf("input hash mismatch was partially reused: %+v", res.Stats)
	}
}

func TestTamperedStagePayloadRejectsWholeSnapshot(t *testing.T) {
	s := testSample(t)
	path := filepath.Join(t.TempDir(), "cache.db")
	store, err := cache.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.New(store).Run(context.Background(), s, yuv.DefaultParams(), opts()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE stages SET payload=x'00' WHERE stage_id=?`, cache.StageYSharpen); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = cache.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	res, err := engine.New(store).Run(context.Background(), s, yuv.DefaultParams(), opts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stats.SnapshotRejected || res.Stats.CacheHitCount != 0 || res.Stats.RecomputeCount != 8 {
		t.Fatalf("tampered snapshot was partially reused: %+v", res.Stats)
	}
}

func TestCancelAndRestartMatchesFullRecompute(t *testing.T) {
	s := testSample(t)
	p1 := yuv.DefaultParams()
	p2 := p1
	p2.ChromaU.Gain = 1.18

	store, _ := openStore(t)
	eng := engine.New(store)
	ctx := context.Background()
	if _, err := eng.RunLive(ctx, s, p1, opts()); err != nil {
		t.Fatal(err)
	}
	cancelOpts := opts()
	cancelOpts.CancelAfterCommits = 1
	partial, err := eng.RunLive(ctx, s, p2, cancelOpts)
	if apperr.Code(err) != apperr.CodeCanceled {
		t.Fatalf("expected E_CANCELLED, got %v", err)
	}
	if partial.Complete || partial.SnapshotID == 0 || len(partial.Committed) != 1 {
		t.Fatalf("invalid cancel boundary: complete=%v snapshot=%d commits=%v",
			partial.Complete, partial.SnapshotID, partial.Committed)
	}

	resumed, err := engine.New(store).Run(ctx, s, p2, opts())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stats.SnapshotRejected {
		t.Fatalf("matching partial snapshot was rejected: %s", resumed.Stats.SnapshotRejectReason)
	}
	if resumed.Stats.CacheHitCount != 7 || resumed.Stats.RecomputeCount != 1 ||
		len(resumed.Recomputed) != 1 || resumed.Recomputed[0] != cache.StageInterleave {
		t.Fatalf("unexpected resume stats: %+v recomputed=%v", resumed.Stats, resumed.Recomputed)
	}

	fullStore, _ := openStore(t)
	full, err := engine.New(fullStore).Run(ctx, s, p2, opts())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.CompareResults(resumed, full); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCountsProduceIdenticalStages(t *testing.T) {
	s := testSample(t)
	p := yuv.DefaultParams()
	run := func(workers int) *engine.RunResult {
		store, _ := openStore(t)
		o := opts()
		o.Workers = workers
		res, err := engine.New(store).Run(context.Background(), s, p, o)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	one := run(1)
	four := run(4)
	if err := engine.CompareResults(one, four); err != nil {
		t.Fatal(err)
	}
}

func TestHeatmapAndReportAreDeterministic(t *testing.T) {
	s := testSample(t)
	store, _ := openStore(t)
	res, err := engine.New(store).Run(context.Background(), s, yuv.DefaultParams(), opts())
	if err != nil {
		t.Fatal(err)
	}
	original, err := yuv.BuildOriginalImage(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	p1 := filepath.Join(t.TempDir(), "a.pgm")
	p2 := filepath.Join(t.TempDir(), "b.pgm")
	h1, err := engine.WriteHeatmap(context.Background(), p1, original, res.Final, 0, 0.25)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := engine.WriteHeatmap(context.Background(), p2, original, res.Final, 0, 0.25)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(p1)
	b, _ := os.ReadFile(p2)
	if !bytes.Equal(a, b) || h1.Digest != h2.Digest {
		t.Fatal("heatmap is not deterministic")
	}
	report, err := engine.BuildErrorReport(context.Background(), s, yuv.DefaultParams(), res)
	if err != nil {
		t.Fatal(err)
	}
	if report.UsedTruthDeviation || len(report.Blocks) != len(s.Metadata.Blocks) {
		t.Fatalf("invalid error report: %+v", report)
	}
	for _, block := range report.Blocks {
		sum := block.ChromaUpsampleError + block.LumaSharpenContrastResidual +
			block.ChromaGainQuantizationError + block.InterleaveRoundingResidual
		if math.Abs(sum-block.CumulativeBoundQuantUnits) > 1e-5 ||
			block.CumulativeBoundQuantUnits <= 0 {
			t.Fatalf("invalid cumulative bound for %s: %+v", block.Block, block)
		}
	}
}

func TestPerWorkerBufferCapProducesRecordedResourceError(t *testing.T) {
	s := testSample(t)
	store, _ := openStore(t)
	o := opts()
	o.BufferBytesPerWorker = 1024
	res, err := engine.New(store).Run(context.Background(), s, yuv.DefaultParams(), o)
	if apperr.Code(err) != apperr.CodeResource {
		t.Fatalf("expected E_RESOURCE, got %v", err)
	}
	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("expected structured error, got %T", err)
	}
	found := false
	for _, event := range res.Stats.BufferWaitEvents {
		if event.Reason == "stage_exceeds_per_worker_buffer_cap" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resource event not recorded: %+v", res.Stats.BufferWaitEvents)
	}
}

func TestCompareLocatesFirstDiffPixel(t *testing.T) {
	s := testSample(t)
	store, _ := openStore(t)
	res, err := engine.New(store).Run(context.Background(), s, yuv.DefaultParams(), opts())
	if err != nil {
		t.Fatal(err)
	}
	clone := *res
	clone.Stages = make(map[string]*cache.Output, len(res.Stages))
	for id, out := range res.Stages {
		clone.Stages[id] = out
	}
	img := *res.Final
	img.Pixels = append([]yuv.Pixel(nil), res.Final.Pixels...)
	x, y := 7, 5
	idx := y*img.Width + x
	img.Pixels[idx].U++
	imgOut := *res.Stages[cache.StageInterleave]
	imgOut.ImageData = &img
	clone.Stages[cache.StageInterleave] = &imgOut
	err = engine.CompareResults(res, &clone)
	if apperr.Code(err) != apperr.CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Stage != cache.StageInterleave ||
		appErr.X == nil || appErr.Y == nil || *appErr.X != x || *appErr.Y != y {
		t.Fatalf("wrong first diff location: %+v", appErr)
	}
}

func TestOutputValidationRejectsWrongTruthGeometry(t *testing.T) {
	s := testSample(t)
	store, _ := openStore(t)
	res, err := engine.New(store).Run(context.Background(), s, yuv.DefaultParams(), opts())
	if err != nil {
		t.Fatal(err)
	}
	bad := *res.Final
	bad.Width--
	if err := engine.ValidateOutput(s, &bad); apperr.Code(err) != apperr.CodePlane {
		t.Fatalf("expected E_PLANE for wrong output geometry, got %v", err)
	}
}

func TestVerifyThreePathsOrchestration(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "sample")
	if _, err := samplegen.Generate(context.Background(), sourceDir,
		samplegen.Config{ID: "verify_test", Width: 96, Height: 64}, 31337); err != nil {
		t.Fatal(err)
	}
	p1 := yuv.DefaultParams()
	p2 := p1
	p2.ChromaU.Gain = 1.14
	p3 := p2
	p3.Luma.SharpenAmount = 0.88
	seq := &engine.EditSequence{
		Name: "test_sequence", ParamVersion: 1, AlignmentVersion: 1,
		Steps: []engine.EditStep{
			{Name: "one", Params: p1},
			{Name: "two", Params: p2},
			{Name: "three", Params: p3},
		},
	}
	report, err := engine.VerifyThreePaths(context.Background(), sourceDir, seq, engine.VerifyOptions{
		WorkDir: filepath.Join(t.TempDir(), "verify"), CancelStep: 2, CancelAfterCommits: 1,
		Workers: 2, BufferMiB: 16, HeatNormMin: 0, HeatNormMax: 0.25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Comparison["status"] != "identical" || report.CancelBoundary.CompleteAfterCancel {
		t.Fatalf("unexpected verification report: %+v", report)
	}
	if len(report.Incremental.CommitOrder) != len(report.Recovery.CommitOrder) {
		t.Fatalf("incremental/recovery commit sequence lengths differ: %v vs %v",
			report.Incremental.CommitOrder, report.Recovery.CommitOrder)
	}
	for i := range report.Incremental.CommitOrder {
		if report.Incremental.CommitOrder[i] != report.Recovery.CommitOrder[i] {
			t.Fatalf("commit sequence differs at %d: %s vs %s", i,
				report.Incremental.CommitOrder[i], report.Recovery.CommitOrder[i])
		}
	}
}

func assertInvalid(t *testing.T, res *engine.RunResult, stage, reason string) {
	t.Helper()
	for _, inv := range res.Invalidations {
		if inv.Stage == stage {
			if inv.Reason != reason {
				t.Fatalf("stage %s reason=%s want=%s", stage, inv.Reason, reason)
			}
			return
		}
	}
	t.Fatalf("stage %s missing from invalidations: %+v", stage, res.Invalidations)
}
