package maskqa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestOrderAdjudicationAndDeterministicMask(t *testing.T) {
	set, err := GenerateSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, fx := range set.Fixtures {
		cfg := SchedulerConfig{MaxParallelism: 4, MaxPixelsPerWorker: 4096, QueueBudgetTiles: 2}
		eng, err := NewEngine(fx.Project, fx.Truth, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := eng.Run(context.Background(), RunOptions{})
		if err != nil {
			t.Fatalf("%s run: %v", fx.Name, err)
		}
		if !res.Mask.Equal(fx.Truth.Mask) {
			tile, p, msg := FirstMaskDifference(res.Mask, fx.Truth.Mask, eng.Tiles)
			t.Fatalf("%s mismatch tile=%d p=%v %s", fx.Name, tile, p, msg)
		}
		if !res.Report.OK {
			t.Fatalf("%s report not OK: %+v", fx.Name, res.Report)
		}

		shuffled := *fx.Project
		shuffled.Strokes = append([]Stroke(nil), fx.Project.Strokes...)
		for i := 0; i < len(shuffled.Strokes); i++ {
			j := (i*7 + 2) % len(shuffled.Strokes)
			shuffled.Strokes[i], shuffled.Strokes[j] = shuffled.Strokes[j], shuffled.Strokes[i]
		}
		eng2, err := NewEngine(&shuffled, fx.Truth, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if eng2.Order.OrderVersion != eng.Order.OrderVersion {
			t.Fatalf("order version changed under array permutation")
		}
		res2, err := eng2.Run(context.Background(), RunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if HashBytes(res2.Mask.Pix) != HashBytes(res.Mask.Pix) {
			t.Fatalf("shuffled declaration order produced different mask")
		}
	}

	fx := set.Fixtures[0]
	conflict := *fx.Project
	strokes := append([]Stroke(nil), fx.Project.Strokes...)
	dup := strokes[0]
	dup.ID = "duplicate"
	strokes = append(strokes, dup)
	conflict.Strokes = strokes
	_, err = NewEngine(&conflict, fx.Truth, DefaultSchedulerConfig(), nil)
	qe, ok := AsError(err)
	if !ok || qe.Code != CodeORDER || len(qe.Conflicts) == 0 {
		t.Fatalf("expected E_ORDER conflict, got %#v", err)
	}
}

func TestRasterParametersAndDeterministicComponent(t *testing.T) {
	set, _ := GenerateSet(DefaultSeed)
	fx := set.Fixtures[0]
	tiles := BuildTiles(fx.Project.Width, fx.Project.Height, DefaultTileSize)
	stroke := fx.Project.Strokes[0]
	a, err := RasterStrokeTile(fx.Project, stroke, tiles[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := RasterStrokeTile(fx.Project, stroke, tiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if a.ContentHash != b.ContentHash || HashComponentAlpha(a.Alpha) != a.ContentHash {
		t.Fatal("same stroke parameters produced non-deterministic component")
	}

	badHardness := stroke
	badHardness.HardnessPermille = 1001
	_, err = RasterStrokeTile(fx.Project, badHardness, tiles[0])
	qe, ok := AsError(err)
	if !ok || qe.Code != CodePARA || qe.Field != "hardness_permille" || qe.LegalRange == "" {
		t.Fatalf("hardness parameter error = %#v", err)
	}

	badWeight := stroke
	badWeight.WeightBPS = WeightBasisScale + 1
	_, err = RasterStrokeTile(fx.Project, badWeight, tiles[0])
	qe, ok = AsError(err)
	if !ok || qe.Code != CodePARA || qe.Field != "weight" || qe.LegalRange == "" {
		t.Fatalf("weight parameter error = %#v", err)
	}
}

func TestTopologyAndDirectionMismatchDetails(t *testing.T) {
	set, _ := GenerateSet(DefaultSeed)
	fx := set.Fixtures[0]
	eng, err := NewEngine(fx.Project, fx.Truth, DefaultSchedulerConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}

	badTruth := *fx.Truth
	badDomains := append([]DomainMetric(nil), fx.Truth.Domains...)
	badDomains[0].Holes = 99
	badDomains[0].RingWidthPx = 1
	badTruth.Domains = badDomains
	_, err = VerifyTopology(res.Mask, &badTruth)
	qe, ok := AsError(err)
	if !ok || qe.Code != CodeTOP || len(qe.Domains) == 0 {
		t.Fatalf("expected E_TOP, got %#v", err)
	}
	foundWidth := false
	for _, issue := range qe.Domains {
		if issue.Field == "ring_width_px" && issue.DomainID == 1 && issue.Actual <= 0 {
			t.Fatalf("ring width issue missing measured value: %+v", issue)
		}
		if issue.Field == "ring_width_px" {
			foundWidth = true
		}
	}
	if !foundWidth {
		t.Fatalf("expected ring_width_px issue, got %+v", qe.Domains)
	}

	badDirTruth := *fx.Truth
	badSegments := append([]BoundarySegment(nil), fx.Truth.Segments...)
	badSegments[0].ExpectedCategory = DirS
	badSegments[0].ExpectedAngleDeg = 90
	badDirTruth.Segments = badSegments
	_, err = VerifyDirections(res.Mask, &badDirTruth)
	qe, ok = AsError(err)
	if !ok || qe.Code != CodeDIR || len(qe.Segments) != 1 {
		t.Fatalf("expected one direction mismatch, got %#v", err)
	}
	issue := qe.Segments[0]
	if issue.SegmentID != badSegments[0].SegmentID || issue.Expected != DirS || issue.MeasuredAngle == 0 && issue.Actual == "" {
		t.Fatalf("direction issue details incomplete: %+v", issue)
	}
}

func TestSchedulerBufferBudgetFailureAndRetry(t *testing.T) {
	tiles := BuildTiles(80, 48, 32)
	cfg := SchedulerConfig{MaxParallelism: 2, MaxPixelsPerWorker: 16, QueueBudgetTiles: 1}
	results, _ := RunTileTasks(context.Background(), tiles, cfg, func(context.Context, Tile, int) (int, error) {
		return 1, nil
	})
	for _, r := range results {
		if r.Status != "failed" || r.ErrorCode != CodePARA {
			t.Fatalf("expected pixel buffer cap failure, got %+v", r)
		}
	}

	var attempts int32
	cfg = SchedulerConfig{MaxParallelism: 2, MaxPixelsPerWorker: 4096, QueueBudgetTiles: 2}
	results, trace := RunTileTasks(context.Background(), tiles, cfg, func(_ context.Context, tile Tile, _ int) (int, error) {
		if tile.ID == 2 && atomic.AddInt32(&attempts, 1) == 1 {
			return 0, errors.New("injected tile failure")
		}
		return tile.ID, nil
	})
	failed := 0
	for _, r := range results {
		if r.Tile.ID == 2 {
			if r.Status != "failed" {
				t.Fatalf("tile 2 status=%s", r.Status)
			}
			failed++
		} else if r.Status != "ok" {
			t.Fatalf("tile %d polluted status=%s err=%v", r.Tile.ID, r.Status, r.Err)
		}
	}
	if failed != 1 {
		t.Fatalf("expected exactly one failed tile")
	}
	retry := RetryTile(context.Background(), tiles[2], cfg, func(_ context.Context, tile Tile, _ int) (int, error) {
		return tile.ID, nil
	})
	if retry.Status != "ok" || retry.Attempts != 2 {
		t.Fatalf("retry result=%+v", retry)
	}
	_ = trace
}

func TestSchedulerQueueBudgetWaitRecorded(t *testing.T) {
	tiles := BuildTiles(128, 96, 32)
	cfg := SchedulerConfig{MaxParallelism: 1, MaxPixelsPerWorker: 4096, QueueBudgetTiles: 1}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	var trace *SchedulerTrace
	go func() {
		_, trace = RunTileTasks(context.Background(), tiles, cfg, func(_ context.Context, tile Tile, _ int) (int, error) {
			if tile.ID == 0 {
				entered <- struct{}{}
				<-release
			}
			return tile.ID, nil
		})
		close(done)
	}()
	<-entered
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-done
	found := false
	for _, w := range trace.QueueWaits {
		if w.Reason == "queue_budget_exhausted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected queue budget wait record, got %+v", trace.QueueWaits)
	}
}

func TestDecodeAndMaskDimensionInjections(t *testing.T) {
	set, _ := GenerateSet(DefaultSeed)
	fx := set.Fixtures[0]
	truncated := fx.Project.ImagePNG[:len(fx.Project.ImagePNG)/2]
	_, err := DecodeImageBytes(truncated)
	qe, ok := AsError(err)
	if !ok || qe.Code != CodeDecode {
		t.Fatalf("truncated image error=%#v", err)
	}

	var doc map[string]any
	_ = json.Unmarshal(fx.Exports[SchemaV3].Data, &doc)
	doc["width_px"] = fx.Project.Width + 1
	doc["image_png_base64"] = base64.StdEncoding.EncodeToString(truncated)
	bad, _ := json.Marshal(doc)
	_, err = LoadProject(bad)
	qe, ok = AsError(err)
	if !ok || qe.Code != CodeDecode || qe.SourceVersion != SchemaV3 {
		t.Fatalf("image injection through loader error=%#v", err)
	}

	_ = json.Unmarshal(fx.Exports[SchemaV3].Data, &doc)
	doc["width_px"] = fx.Project.Width + 1
	bad, _ = json.Marshal(doc)
	_, err = LoadProject(bad)
	qe, ok = AsError(err)
	if !ok || qe.Code != CodePARA || qe.Field != "candidate_mask_png_base64.size" {
		t.Fatalf("mask dimension contradiction error=%#v", err)
	}
}
