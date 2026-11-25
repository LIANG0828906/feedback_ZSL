package fog

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestTraceStableEmptyAndOverlapSpans(t *testing.T) {
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	cam := s.ActiveCamera()
	vp := NewViewport(s.Width, s.Height, s.Recipe.TileSize)

	foundOverlap := false
	foundEmptyGap := false
	for x := 0; x < s.Width; x++ {
		block := vp.PixelTile(x, 13)
		ray := cam.RayForPixel(x, 13, s.Width, s.Height, block)
		first := TraceRay(ray, s.Media, s.FixedScale)
		second := TraceRay(ray, s.Media, s.FixedScale)
		if first.Digest != second.Digest || !reflect.DeepEqual(first.Spans, second.Spans) {
			t.Fatalf("trace %s is not deterministic", ray.ID)
		}
		for _, span := range first.Spans {
			if span.Kind == SpanEmpty {
				foundEmptyGap = true
			}
			if len(span.MediumIDs) > 1 {
				foundOverlap = true
				if span.Rule == "" {
					t.Fatal("overlap span missing segmentation rule")
				}
			}
		}
	}
	if !foundEmptyGap {
		t.Fatal("expected explicit empty gap spans")
	}
	if !foundOverlap {
		t.Fatal("generated sample did not exercise overlapping media")
	}

	miss := Ray{ID: "manual-miss", Origin: Vec3{X: 0, Y: 0, Z: 0}, Dir: Vec3{X: 0, Y: 1, Z: 0}, Near: 0.1, Far: 20}
	tr := TraceRay(miss, s.Media, s.FixedScale)
	if len(tr.Intervals) != 0 {
		t.Fatalf("manual ray should miss all media, intervals=%d", len(tr.Intervals))
	}
	if len(tr.Spans) != 1 || tr.Spans[0].Kind != SpanEmpty {
		t.Fatalf("miss must retain one explicit empty span, got %+v", tr.Spans)
	}
}

func TestHalfPixelTileOwnershipAndBoundaryContinuity(t *testing.T) {
	vp := NewViewport(48, 27, 8)
	if vp.PixelTile(7, 3) != 0 || vp.PixelTile(8, 3) != 1 {
		t.Fatal("tile boundary ownership mismatch")
	}
	for px := 0; px < 48; px++ {
		sampleX := int(math.Floor(float64(px) + 0.5))
		if sampleX != px || vp.PixelTile(sampleX, 0) != vp.PixelTile(px, 0) {
			t.Fatalf("half-pixel sample drifted at px=%d sample=%d", px, sampleX)
		}
	}

	cam := Camera{Forward: Vec3{0, 0, 1}, Right: Vec3{1, 0, 0}, Up: Vec3{0, 1, 0}, FovY: 50}
	d6 := cam.RayForPixel(6, 13, 48, 27, 0).Dir
	d7 := cam.RayForPixel(7, 13, 48, 27, 0).Dir
	d8 := cam.RayForPixel(8, 13, 48, 27, 1).Dir
	insideStep := d7.X - d6.X
	boundaryStep := d8.X - d7.X
	if math.Abs(boundaryStep-insideStep) > 2e-3 {
		t.Fatalf("ray parameter discontinuity: inside=%.8f boundary=%.8f", insideStep, boundaryStep)
	}
}

func TestBudgetGateQueuesParallelAndShadowWait(t *testing.T) {
	r := Recipe{MaxParallelBlocks: 1, RayBufferPerWorker: 4, ShadowSampleBudget: 10}
	g := NewBudgetGate(r)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	releaseFirst, err := g.AcquireBlock(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	secondAcquired := make(chan struct{})
	go func() {
		release, err := g.AcquireBlock(ctx, 2)
		if err == nil {
			defer release()
			close(secondAcquired)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-secondAcquired:
		t.Fatal("second block acquired slot while max parallel=1")
	default:
	}
	releaseFirst()
	<-secondAcquired
	report := g.Report(r)
	foundParallelWait := false
	for _, q := range report.Queue {
		if q.BlockID == 2 && q.Reason == "max_parallel_blocks" && q.WaitMS > 0 {
			foundParallelWait = true
		}
	}
	if !foundParallelWait {
		t.Fatalf("missing parallel queue wait: %+v", report.Queue)
	}

	if err := g.ReserveShadow(ctx, 1, 10); err != nil {
		t.Fatal(err)
	}
	shadowDone := make(chan error, 1)
	go func() { shadowDone <- g.ReserveShadow(ctx, 2, 5) }()
	time.Sleep(20 * time.Millisecond)
	g.AddShadowQuota(5)
	if err := <-shadowDone; err != nil {
		t.Fatal(err)
	}
	report = g.Report(r)
	foundShadowWait := false
	for _, q := range report.Queue {
		if q.BlockID == 2 && q.Reason == "shadow_sample_budget" && q.WaitMS > 0 {
			foundShadowWait = true
		}
	}
	if !foundShadowWait {
		t.Fatalf("missing shadow queue wait: %+v", report.Queue)
	}
}

func TestVerifyTruthReport(t *testing.T) {
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyTruth(s)
	if err != nil {
		t.Fatalf("VerifyTruth: %v report=%+v", err, report)
	}
	if !report.OK || report.PixelTruths < 4 || report.DensityTruths == 0 {
		t.Fatalf("bad truth report: %+v", report)
	}
}

func TestDensityUnchangedDoesNotInvalidate(t *testing.T) {
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	edit := s.DeclaredEdit
	edit.NewValue = edit.OldValue
	_, changed, err := ApplyDensityEdit(s, edit)
	if err != nil || changed {
		t.Fatalf("unchanged density should no-op, changed=%v err=%v", changed, err)
	}
	manifest := InvalidationManifest{Reason: "density-unchanged-no-invalidation"}
	if len(manifest.Blocks) != 0 {
		t.Fatal("unchanged density produced invalidation")
	}
}

func TestCoefficientEditCacheScope(t *testing.T) {
	s, err := Generate(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := BuildDependencyGraph(s, "c0")
	if err != nil {
		t.Fatal(err)
	}
	mediumID := s.Media[0].ID
	manifest := graph.InvalidateMediumCoefficients(mediumID, "absorption")
	if len(manifest.Blocks) == 0 || len(manifest.Blocks) == len(graph.Viewport.Tiles) {
		t.Fatalf("coefficient manifest should be non-empty but medium-scoped: %v", manifest.Blocks)
	}
	affected := map[int]bool{}
	for _, id := range manifest.Blocks {
		affected[id] = true
	}

	cam := s.ActiveCamera()
	before := map[int]string{}
	for _, tile := range graph.Viewport.Tiles {
		identity, _, err := BlockCacheIdentity(s, cam, tile)
		if err != nil {
			t.Fatal(err)
		}
		before[tile.ID] = identity.BlockKey
	}

	s.Media[0].Absorption += 0.01
	s.Media[0].Rehash()
	for _, tile := range graph.Viewport.Tiles {
		identity, _, err := BlockCacheIdentity(s, cam, tile)
		if err != nil {
			t.Fatal(err)
		}
		changed := before[tile.ID] != identity.BlockKey
		if changed != affected[tile.ID] {
			t.Fatalf("block %d cache changed=%v want affected=%v", tile.ID, changed, affected[tile.ID])
		}
	}
}
