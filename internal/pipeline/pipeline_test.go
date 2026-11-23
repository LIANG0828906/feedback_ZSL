package pipeline_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"glyphbrush/internal/checkpoint"
	"glyphbrush/internal/errs"
	"glyphbrush/internal/geom"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/pipeline"
	"glyphbrush/internal/raster"
	"glyphbrush/internal/samplegen"
	"glyphbrush/internal/stroke"
)

func testBundle(t *testing.T) *loader.Bundle {
	t.Helper()
	dir := t.TempDir()
	if _, _, err := samplegen.Write(dir, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	bundle, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func testEngine(t *testing.T, bundle *loader.Bundle, glyphIndex int) *pipeline.Engine {
	t.Helper()
	return testEngineWithStore(t, bundle, glyphIndex, checkpoint.New(t.TempDir()))
}

func testEngineWithStore(t *testing.T, bundle *loader.Bundle, glyphIndex int, store *checkpoint.Store) *pipeline.Engine {
	t.Helper()
	return testEngineWithConfig(t, bundle, glyphIndex, func(cfg *pipeline.Config) {
		cfg.Store = store
	})
}

func testEngineWithConfig(t *testing.T, bundle *loader.Bundle, glyphIndex int, mutate func(*pipeline.Config)) *pipeline.Engine {
	t.Helper()
	cfg := pipeline.DefaultConfig()
	cfg.DensityRange = bundle.Document.DensityRange
	cfg.CoordinateRange = bundle.Document.CoordinateRange
	if mutate != nil {
		mutate(&cfg)
	}
	engine, err := pipeline.New(bundle.Glyphs[glyphIndex], cfg)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestFourFaultCodesAndRetry(t *testing.T) {
	cases := []struct {
		kind pipeline.FaultKind
		code errs.Code
	}{
		{pipeline.FaultSelf, errs.CodeSelf},
		{pipeline.FaultZero, errs.CodeZero},
		{pipeline.FaultDensity, errs.CodeDens},
		{pipeline.FaultStage, errs.CodeStage},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			bundle := testBundle(t)
			engine := testEngine(t, bundle, 0)
			if _, err := engine.Warm(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := engine.SetFaults([]pipeline.Fault{{SegmentID: 2, Kind: tc.kind}}); err != nil {
				t.Fatal(err)
			}
			failed, err := engine.Retry(context.Background(), 2)
			if !errs.IsCode(err, tc.code) {
				t.Fatalf("expected %s, got %v", tc.code, err)
			}
			e, ok := errs.As(err)
			if !ok || e.SegmentID != 2 || e.GlyphID == "" || e.Param == "" {
				t.Fatalf("incomplete error context: %+v", err)
			}
			if len(failed.Segments) < 2 {
				t.Fatal("failed segment polluted/removed other segments")
			}
			for _, segment := range failed.Segments {
				if segment.SegmentID != 2 && segment.Status != pipeline.StatusValid {
					t.Fatalf("unrelated segment %d was polluted", segment.SegmentID)
				}
			}
			if tc.kind == pipeline.FaultStage {
				retried, err := engine.Retry(context.Background(), 2)
				if err != nil {
					t.Fatalf("one-shot stage retry failed: %v", err)
				}
				full, err := engine.FullRender(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pipeline.Compare(retried, full); err != nil {
					t.Fatalf("retry differs from full: %v", err)
				}
			} else {
				engine.ClearFault(2)
				retried, err := engine.Retry(context.Background(), 2)
				if err != nil {
					t.Fatalf("retry after clearing fault failed: %v", err)
				}
				full, err := engine.FullRender(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pipeline.Compare(retried, full); err != nil {
					t.Fatalf("retry differs from full: %v", err)
				}
			}
		})
	}
}

func TestCompareLocatesPixelAndSegment(t *testing.T) {
	bundle := testBundle(t)
	engine := testEngine(t, bundle, 0)
	full, err := engine.FullRender(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	incremental := *full
	incremental.Mask = raster.Mask{
		Width:  full.Mask.Width,
		Height: full.Mask.Height,
		Data:   append([]byte(nil), full.Mask.Data...),
	}
	x, y, owner := firstOwnedPixel(full)
	if owner == 0 {
		t.Fatal("no owned pixel found")
	}
	incremental.Mask.Data[y*incremental.Mask.Width+x] ^= 0xff
	diff, err := pipeline.Compare(&incremental, full)
	if !errs.IsCode(err, errs.CodeDiff) {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	if diff.PixelX != x || diff.PixelY != y || diff.SegmentID <= 0 {
		t.Fatalf("bad diff context: %+v", diff)
	}
	e, _ := errs.As(err)
	if e.PixelX == nil || e.PixelY == nil || e.SegmentID == 0 {
		t.Fatalf("error lacks pixel/segment: %+v", e)
	}
}

func TestCompareLocatesDensityMismatch(t *testing.T) {
	bundle := testBundle(t)
	engine := testEngine(t, bundle, 0)
	full, err := engine.FullRender(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	incremental := *full
	incremental.Densities = append([]raster.DensitySample(nil), full.Densities...)
	if len(incremental.Densities) == 0 {
		t.Fatal("no density samples")
	}
	incremental.Densities[0].DensityPPM++
	diff, err := pipeline.Compare(&incremental, full)
	if !errs.IsCode(err, errs.CodeDiff) || diff.SegmentID <= 0 || diff.Kind != "density" {
		t.Fatalf("expected density E_DIFF with segment, got diff=%+v err=%v", diff, err)
	}
}

func TestCompareLocatesBoundaryMismatch(t *testing.T) {
	bundle := testBundle(t)
	engine := testEngine(t, bundle, 0)
	full, err := engine.FullRender(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	incremental := *full
	incremental.Densities = append([]raster.DensitySample(nil), full.Densities...)
	if len(incremental.Densities) == 0 || len(incremental.Densities[0].Boundary) == 0 {
		t.Fatal("no boundary samples")
	}
	incremental.Densities[0].Boundary = append([]geom.Point(nil), incremental.Densities[0].Boundary...)
	incremental.Densities[0].Boundary[0].X++
	diff, err := pipeline.Compare(&incremental, full)
	if !errs.IsCode(err, errs.CodeDiff) || diff.SegmentID <= 0 || diff.Kind != "density" {
		t.Fatalf("expected boundary E_DIFF with segment, got diff=%+v err=%v", diff, err)
	}
}

func TestCheckpointRestoreAndWholeSnapshotInvalidation(t *testing.T) {
	bundle := testBundle(t)
	store := checkpoint.New(t.TempDir())
	engine := testEngineWithStore(t, bundle, 0, store)
	warm, err := engine.Warm(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(store.Root, bundle.Glyphs[0].Glyph.ID, "segments", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != len(warm.Segments) {
		t.Fatalf("checkpoint has %d segment files, want %d", len(matches), len(warm.Segments))
	}
	restored := testEngineWithStore(t, bundle, 0, store)
	ok, err := restored.Restore()
	if err != nil || !ok {
		t.Fatalf("restore failed: ok=%v err=%v", ok, err)
	}
	refreshed := restored.Refresh()
	if _, err := pipeline.Compare(refreshed, warm); err != nil {
		t.Fatalf("restored cache differs: %v", err)
	}

	changedCfg := pipeline.DefaultConfig()
	changedCfg.DensityRange = bundle.Document.DensityRange
	changedCfg.CoordinateRange = bundle.Document.CoordinateRange
	changedCfg.Store = store
	changedCfg.Params.Width += 6
	changed, err := pipeline.New(bundle.Glyphs[0], changedCfg)
	if err != nil {
		t.Fatal(err)
	}
	ok, err = changed.Restore()
	if ok || !errors.Is(err, checkpoint.ErrVersionMismatch) {
		t.Fatalf("expected version mismatch, got ok=%v err=%v", ok, err)
	}
	if _, statErr := os.Stat(filepath.Join(store.Root, bundle.Glyphs[0].Glyph.ID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("snapshot was not discarded: %v", statErr)
	}
}

func firstOwnedPixel(result *pipeline.RunResult) (int, int, int) {
	for y := 0; y < result.Mask.Height; y++ {
		for x := 0; x < result.Mask.Width; x++ {
			if result.Mask.At(x, y) == 0 {
				continue
			}
			for _, segment := range result.Segments {
				if segment.Mask.At(x, y) != 0 {
					return x, y, segment.SegmentID
				}
			}
		}
	}
	return 0, 0, 0
}

func TestWarmIncrementalAndFullMatch(t *testing.T) {
	bundle := testBundle(t)
	engine := testEngine(t, bundle, 0)
	warm, err := engine.Warm(context.Background())
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if len(warm.Segments) == 0 {
		t.Fatal("warm produced no segments")
	}
	resultByID := make(map[int]pipeline.SegmentResult, len(warm.Segments))
	for _, segment := range warm.Segments {
		resultByID[segment.SegmentID] = segment
	}
	for _, plan := range engine.Plans() {
		segment := resultByID[plan.SegmentID]
		if len(segment.Densities) != len(plan.Skeleton) {
			t.Fatalf("segment %d has %d densities for %d skeleton points",
				plan.SegmentID, len(segment.Densities), len(plan.Skeleton))
		}
		for _, sample := range segment.Densities {
			if sample.DensityPPM < 0 || sample.DensityPPM > 1000 || len(sample.Boundary) != 2 {
				t.Fatalf("invalid density sample: %+v", sample)
			}
		}
	}
	next := engine.Params()
	next.Width += 6
	incremental, err := engine.UpdateParams(context.Background(), next)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	full, err := engine.FullRender(context.Background())
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if _, err := pipeline.Compare(incremental, full); err != nil {
		t.Fatalf("incremental differs from full: %v", err)
	}
	if incremental.CompositeHash != full.CompositeHash {
		t.Fatal("composite hashes differ")
	}
}

func TestAllStrokeParametersPreserveFullConsistency(t *testing.T) {
	changes := []struct {
		name string
		edit func(*stroke.Params)
	}{
		{"width", func(p *stroke.Params) { p.Width += 4 }},
		{"spacing", func(p *stroke.Params) { p.Spacing *= 0.8 }},
		{"join", func(p *stroke.Params) { p.Join = stroke.JoinBevel }},
		{"tip", func(p *stroke.Params) { p.Tip = stroke.TipSquare }},
		{"curvatureFactor", func(p *stroke.Params) { p.CurvatureFactor += 0.1 }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			bundle := testBundle(t)
			engine := testEngine(t, bundle, 1)
			if _, err := engine.Warm(context.Background()); err != nil {
				t.Fatal(err)
			}
			next := engine.Params()
			tc.edit(&next)
			incremental, err := engine.UpdateParams(context.Background(), next)
			if err != nil {
				t.Fatal(err)
			}
			full, err := engine.FullRender(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pipeline.Compare(incremental, full); err != nil {
				t.Fatalf("incremental differs from full: %v", err)
			}
		})
	}
}

func TestParallelismDoesNotChangePixelsOrOrder(t *testing.T) {
	bundle := testBundle(t)
	serial := testEngineWithConfig(t, bundle, 2, func(cfg *pipeline.Config) { cfg.MaxParallel = 1 })
	parallel := testEngineWithConfig(t, bundle, 2, func(cfg *pipeline.Config) { cfg.MaxParallel = 8 })
	a, err := serial.FullRender(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := parallel.FullRender(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.CompositeHash != b.CompositeHash {
		t.Fatal("parallel composite hash differs from serial")
	}
	if _, err := pipeline.Compare(a, b); err != nil {
		t.Fatal(err)
	}
	for i := range a.Segments {
		if a.Segments[i].SegmentID != b.Segments[i].SegmentID {
			t.Fatal("segment order is not stable")
		}
	}
}

func TestParameterValidation(t *testing.T) {
	bundle := testBundle(t)
	engine := testEngine(t, bundle, 0)
	next := engine.Params()
	next.Width = 0
	_, err := engine.UpdateParams(context.Background(), next)
	if !errs.IsCode(err, errs.CodeParam) {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	next = engine.Params()
	next.Spacing = 0
	_, err = engine.UpdateParams(context.Background(), next)
	if !errs.IsCode(err, errs.CodeParam) {
		t.Fatalf("expected E_PARA, got %v", err)
	}
}

func TestMultipleChangedParametersAreAllReported(t *testing.T) {
	bundle := testBundle(t)
	engine := testEngine(t, bundle, 0)
	if _, err := engine.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := engine.Params()
	next.Width += 2
	next.Tip = stroke.TipFlat
	result, err := engine.UpdateParams(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range result.Validation.Entries {
		seen[entry.Param] = true
	}
	if !seen["width"] || !seen["tip"] {
		t.Fatalf("missing changed parameter in invalidation report: %+v", result.Validation.Entries)
	}
	if len(result.Validation.Entries) != 2*len(result.Segments) {
		t.Fatalf("expected two entries per segment, got %d entries for %d segments",
			len(result.Validation.Entries), len(result.Segments))
	}
}

func TestControlEditOnlyInvalidatesDependentContourTail(t *testing.T) {
	bundle := testBundle(t)
	engine := testEngine(t, bundle, 0)
	if _, err := engine.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := engine.Plans()
	result, err := engine.EditControl(context.Background(), pipeline.ControlEdit{
		ContourIndex: 0, SegmentIndex: 0, ControlIndex: 0, DX: 2, DY: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Validation == nil || len(result.Validation.Entries) == 0 {
		t.Fatal("expected invalidation entries")
	}
	for _, entry := range result.Validation.Entries {
		if entry.ContourIndex != 0 {
			t.Fatalf("unrelated contour invalidated: %+v", entry)
		}
	}
	if len(result.Reused) == 0 {
		t.Fatal("expected unrelated segments to be reused")
	}
	if len(result.Redrawn) >= len(before) {
		t.Fatalf("expected partial redraw, got %d/%d", len(result.Redrawn), len(before))
	}
	full, err := engine.FullRender(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Compare(result, full); err != nil {
		t.Fatalf("control-edit incremental differs from full: %v", err)
	}
}
