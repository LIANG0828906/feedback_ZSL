package vpf

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func testBundle(t *testing.T) *SampleBundle {
	t.Helper()
	bundle, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatalf("GenerateSample: %v", err)
	}
	return bundle
}

func TestThreeSchemaLoadersAndTreeEquivalence(t *testing.T) {
	bundle := testBundle(t)
	if len(bundle.Documents) != 3 || len(bundle.Loaded) != 3 {
		t.Fatalf("expected three schema documents and loaders")
	}
	for version := SchemaV1; version <= SchemaV3; version++ {
		p := bundle.Loaded[version]
		if p.SourceVersion != version {
			t.Fatalf("v%d source version = %d", version, p.SourceVersion)
		}
		if got := ActiveLayerIDs(p); len(got) != 3 || got[0] != "ground_base" || got[1] != "ripple_mask" || got[2] != "slate_badge" {
			t.Fatalf("v%d active stack = %v", version, got)
		}
		if len(p.Traces.Fields) < 20 {
			t.Fatalf("v%d expected field traces, got %d", version, len(p.Traces.Fields))
		}
		diff, err := CompareExpressions(bundle.Loaded[SchemaV3], p)
		if err != nil {
			t.Fatalf("v%d expression mismatch: %v diff=%+v", version, err, diff)
		}
	}

	truthData, err := json.Marshal(bundle.Truth)
	if err != nil {
		t.Fatal(err)
	}
	truth, err := LoadTruth(truthData)
	if err != nil {
		t.Fatal(err)
	}
	if len(truth.Frames) != 3 || len(truth.Frames[0].SampleWeights) != 6 {
		t.Fatalf("unexpected truth frame/sample count: %+v", truth.Frames)
	}
	var center *WeightSampleTruth
	for i := range truth.Frames[0].SampleWeights {
		sample := &truth.Frames[0].SampleWeights[i]
		if sample.SourceID == "r_left" && sample.X == 28 && sample.Y == 28 {
			center = sample
		}
	}
	if center == nil || center.ExpectedWeightBP != 9000 {
		t.Fatalf("center truth = %+v, want weight 9000", center)
	}
}

func TestSchemaErrorsPointAtFieldAndVersion(t *testing.T) {
	_, err := LoadProject([]byte(`{"version":9}`))
	if !HasCode(err, CodeSchema) {
		t.Fatalf("unknown version error = %v", err)
	}
	ve, _ := AsError(err)
	if ve.Version != 9 || ve.FieldPath != "$.version" {
		t.Fatalf("unknown version diagnostic = %+v", ve)
	}

	bundle := testBundle(t)
	var doc map[string]any
	if err := json.Unmarshal(bundle.Documents[SchemaV1], &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "project_id")
	data, _ := json.Marshal(doc)
	_, err = LoadProject(data)
	if !HasCode(err, CodeSchema) {
		t.Fatalf("missing field error = %v", err)
	}
	ve, _ = AsError(err)
	if ve.Version != 1 || ve.FieldPath != "$.project_id" {
		t.Fatalf("missing field diagnostic = %+v", ve)
	}
}

func TestIntegerConversionsOverflowTruncationAndRoundTrip(t *testing.T) {
	var traces TraceBook
	ns, err := TicksToNanoseconds("$.test.tick", 40, &traces)
	if err != nil || ns != 20*MillisecondNanoseconds {
		t.Fatalf("tick conversion ns=%d err=%v", ns, err)
	}
	ticks, err := NanosecondsToTicks("$.test.ns", ns, &traces)
	if err != nil || ticks != 40 {
		t.Fatalf("tick round trip = %d, %v", ticks, err)
	}

	traces = TraceBook{}
	ticks, err = NanosecondsToTicks("$.test.half_tick", TickNanoseconds/2, &traces)
	if err != nil || ticks != 0 || len(traces.Truncations) != 1 || traces.Truncations[0].Remainder == 0 {
		t.Fatalf("truncation traces = %+v ticks=%d err=%v", traces.Truncations, ticks, err)
	}

	_, err = TicksToNanoseconds("$.overflow.tick", math.MaxInt64, nil)
	if !HasCode(err, CodeOVF) {
		t.Fatalf("overflow error = %v", err)
	}
	oe, _ := AsError(err)
	if oe.FieldPath != "$.overflow.tick" || oe.Left != int64(math.MaxInt64) || oe.Right != TickNanoseconds {
		t.Fatalf("overflow diagnostic missing operands: %+v", oe)
	}

	for _, channel := range []uint8{0, 64, 80, 200, 255} {
		premult, err := PremultiplyChannel("$.c", channel, 204, nil)
		if err != nil {
			t.Fatal(err)
		}
		straight, err := UnpremultiplyChannel("$.c", premult, 204, nil)
		if err != nil {
			t.Fatal(err)
		}
		if straight != channel && channel%5 == 0 {
			t.Fatalf("safe channel %d did not round-trip: premult=%d straight=%d", channel, premult, straight)
		}
	}
}

func TestHighLowHighExportKeepsChannelsAndTime(t *testing.T) {
	bundle := testBundle(t)
	high := bundle.Loaded[SchemaV3]
	data, report, err := ExportProject(high, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if report == nil || report.Code != CodeLossy || len(report.Items) == 0 {
		t.Fatalf("expected E_LOSSY report, got %+v", report)
	}
	if _, err := ExportProjectStrict(high, SchemaV1); !HasCode(err, CodeLossy) {
		t.Fatalf("strict export error = %v", err)
	}
	foundGroupLoss := false
	for _, item := range report.Items {
		if item.Kind == "group_hierarchy" {
			foundGroupLoss = true
		}
	}
	if !foundGroupLoss {
		t.Fatalf("report did not include group hierarchy loss: %+v", report.Items)
	}
	low, err := LoadProject(data)
	if err != nil {
		t.Fatalf("exported v1 is not readable: %v", err)
	}
	v3Data, _, err := ExportProject(low, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	backHigh, err := LoadProject(v3Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, before := range high.Layers {
		after, ok := backHigh.LayerByID(before.ID)
		if !ok {
			t.Fatalf("layer %s missing after round trip", before.ID)
		}
		if after.Color != before.Color || after.OpacityBP != before.OpacityBP {
			t.Fatalf("layer %s parameter drift: before=%+v after=%+v", before.ID, before, after)
		}
	}
	for i := range high.Frames {
		if high.Frames[i].AtNS != backHigh.Frames[i].AtNS {
			t.Fatalf("frame %d time drift: %d != %d", i, high.Frames[i].AtNS, backHigh.Frames[i].AtNS)
		}
	}
}

func TestTreeMismatchReturnsECodeAndFirstLayer(t *testing.T) {
	bundle := testBundle(t)
	p := bundle.Loaded[SchemaV3]
	other := *p
	other.Resolved = append([]ResolvedLayer(nil), p.Resolved...)
	other.Resolved[0], other.Resolved[1] = other.Resolved[1], other.Resolved[0]
	_, err := CompareExpressions(p, &other)
	if !HasCode(err, CodeTree) {
		t.Fatalf("expected E_TREE, got %v", err)
	}
	te, _ := AsError(err)
	if te.FieldPath != "$.resolved[0]" || te.Left != "ground_base" || te.Right != "ripple_mask" {
		t.Fatalf("unexpected tree diagnostic: %+v", te)
	}
}

func TestRenderIsPixelDeterministic(t *testing.T) {
	bundle := testBundle(t)
	p := bundle.Loaded[SchemaV3]
	first, err := RenderChunk(p, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderChunk(p, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if first.PixelDigest != second.PixelDigest || string(first.Pix) != string(second.Pix) {
		t.Fatal("same chunk rendered different pixels")
	}
}

func TestThreePathsPixelAndSequenceIdentical(t *testing.T) {
	bundle := testBundle(t)
	report, err := VerifyThreePaths(context.Background(), bundle.Loaded[SchemaV2], 1, func() CheckpointStore {
		return NewMemoryStore()
	})
	if err != nil {
		if report != nil {
			t.Fatalf("VerifyThreePaths: %v mismatch=%+v", err, report.FirstMismatch)
		}
		t.Fatalf("VerifyThreePaths: %v", err)
	}
	if !report.PixelIdentical || !report.SequenceIdentical {
		t.Fatalf("three-path report not identical: %+v", report)
	}
	if len(report.RecoveryReused) != 2 || report.RecoveryReused[0] != 0 || report.RecoveryReused[1] != 1 {
		t.Fatalf("recovery reuse = %v", report.RecoveryReused)
	}
}

func TestIncrementalPreciseInvalidationScenarios(t *testing.T) {
	ctx := context.Background()

	t.Run("opacity_badge_only_chunk_zero", func(t *testing.T) {
		p := testBundle(t).Loaded[SchemaV3]
		report, err := VerifyIncrementalEdit(ctx, p, 0, NewMemoryStore(), func(edited *Project) (Change, error) {
			return SetLayerOpacity(edited, "slate_badge", 4200)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := report.Plan.InvalidChunks; len(got) != 1 || got[0] != 0 {
			t.Fatalf("invalid chunks = %v, want [0]", got)
		}
		if len(report.Plan.ReusableChunks) != 5 || !report.UnaffectedStable {
			t.Fatalf("reusable=%v stable=%v", report.Plan.ReusableChunks, report.UnaffectedStable)
		}
	})

	t.Run("group_visibility_overlay_chunk_zero", func(t *testing.T) {
		p := testBundle(t).Loaded[SchemaV2]
		report, err := VerifyIncrementalEdit(ctx, p, 0, NewMemoryStore(), func(edited *Project) (Change, error) {
			return SetGroupVisibility(edited, "overlay_group", false)
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Plan.InvalidChunks) != 1 || report.Plan.InvalidChunks[0] != 0 {
			t.Fatalf("group invalid chunks = %v", report.Plan.InvalidChunks)
		}
	})

	t.Run("ripple_source_left_four_tiles", func(t *testing.T) {
		p := testBundle(t).Loaded[SchemaV1]
		report, err := VerifyIncrementalEdit(ctx, p, 0, NewMemoryStore(), func(edited *Project) (Change, error) {
			return EditRippleSource(edited, "r_left", nil, 0, 0, 4321)
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []int{0, 1, 3, 4}
		if len(report.Plan.InvalidChunks) != len(want) {
			t.Fatalf("ripple invalid = %v, want %v", report.Plan.InvalidChunks, want)
		}
		for i := range want {
			if report.Plan.InvalidChunks[i] != want[i] {
				t.Fatalf("ripple invalid = %v, want %v", report.Plan.InvalidChunks, want)
			}
		}
	})
}

func TestChunkFailureIsolationAndRetry(t *testing.T) {
	ctx := context.Background()
	p := testBundle(t).Loaded[SchemaV3]
	store := NewMemoryStore()
	runner := &Runner{
		Project: p, FrameIndex: 0, Store: store,
		Scheduler: NewBoundedScheduler(SchedulerOptions{MaxParallel: 2, QueueBudget: 1, MaxBufferedPixels: 2048, Retries: 0}),
	}
	_, err := runner.Build(ctx, RunOptions{
		Path: "with-failure",
		FailBeforeRender: func(chunkIndex, attempt int) error {
			if chunkIndex == 2 {
				return errors.New("injected deterministic chunk failure")
			}
			return nil
		},
	})
	if !HasCode(err, CodeChunk) {
		t.Fatalf("expected chunk error, got %v", err)
	}
	records, _ := store.ListCheckpoints(ctx, 0)
	if len(records) != p.ChunkCount()-1 {
		t.Fatalf("committed records = %d, want only successful chunks", len(records))
	}
	if _, found, _ := store.GetCheckpoint(ctx, 0, 2); found {
		t.Fatal("failed chunk must not be checkpointed")
	}

	retryRunner := &Runner{
		Project: p, FrameIndex: 0, Store: store,
		Scheduler: NewBoundedScheduler(SchedulerOptions{MaxParallel: 1, QueueBudget: 1, MaxBufferedPixels: 2048, Retries: 1}),
	}
	retried, err := retryRunner.Build(ctx, RunOptions{Path: "retry", ChunkIndices: []int{2}, ReuseStored: true})
	if err != nil {
		t.Fatalf("retry chunk: %v", err)
	}
	if len(retried.Commits) != 1 || retried.Commits[0].PixelDigest == "" {
		t.Fatalf("retry result = %+v", retried.Commits)
	}
	all, err := retryRunner.Build(ctx, RunOptions{Path: "assemble-after-retry", ReuseStored: true})
	if err != nil {
		t.Fatal(err)
	}
	if all.FrameDigest == "" {
		t.Fatal("expected assembled frame after retry")
	}
}

func TestSchedulerQueueBudgetWaitRecorded(t *testing.T) {
	scheduler := NewBoundedScheduler(SchedulerOptions{MaxParallel: 1, QueueBudget: 1, MaxBufferedPixels: 16, Retries: 0})
	release := make(chan struct{})
	runDone := make(chan struct{})
	var report *SchedulerReport
	go func() {
		_, report, _ = scheduler.Run(context.Background(), []int{0, 1, 2}, func(index, workerID int, buffer *PixelBuffer, attempt int) (*ChunkResult, error) {
			if index == 0 {
				<-release
			}
			return &ChunkResult{ChunkIndex: index}, nil
		})
		close(runDone)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not finish")
	}
	if report == nil || len(report.QueueWaits) == 0 {
		t.Fatalf("expected queue wait record, report=%+v", report)
	}
	if report.QueueWaits[0].Reason != "pending_queue_budget_exhausted" {
		t.Fatalf("unexpected queue wait reason: %+v", report.QueueWaits[0])
	}

	smallBuffer := NewPixelBuffer(10)
	err := smallBuffer.use(1024)
	if !HasCode(err, CodeBudget) {
		t.Fatalf("pixel buffer budget error = %v", err)
	}
}

func TestDefaultFillTraceRecordsOldAndDefault(t *testing.T) {
	bundle := testBundle(t)
	var doc map[string]any
	if err := json.Unmarshal(bundle.Documents[SchemaV1], &doc); err != nil {
		t.Fatal(err)
	}
	layers := doc["layers"].([]any)
	ground := layers[0].(map[string]any)
	delete(ground, "opacity_pct")
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadProject(data)
	if err != nil {
		t.Fatal(err)
	}
	layer, _ := p.LayerByID("ground_base")
	if layer.OpacityBP != 10000 {
		t.Fatalf("default opacity BP = %d", layer.OpacityBP)
	}
	found := false
	for _, trace := range p.Traces.Fields {
		if trace.SourcePath == "$.layers[0].opacity_pct" && trace.Transform == "default_fill" {
			found = true
			if trace.DefaultValue != int64(100) || trace.OldValue != nil {
				t.Fatalf("unexpected default trace: %+v", trace)
			}
		}
	}
	if !found {
		t.Fatal("missing opacity default_fill trace")
	}
}

func TestCompareFramesLocatesChunkAndPixel(t *testing.T) {
	p := testBundle(t).Loaded[SchemaV3]
	chunks := map[int]*ChunkResult{}
	for i := 0; i < p.ChunkCount(); i++ {
		chunk, err := RenderChunk(p, 0, i)
		if err != nil {
			t.Fatal(err)
		}
		chunks[i] = chunk
	}
	frame, err := AssembleFrame(p, 0, chunks)
	if err != nil {
		t.Fatal(err)
	}
	altered := &FrameBuffer{Width: frame.Width, Height: frame.Height, Channels: frame.Channels, Pix: append([]byte(nil), frame.Pix...)}
	x, y := 40, 40
	offset := (y*p.Width + x) * 4
	altered.Pix[offset] ^= 0xff
	mismatch := CompareFrames(p, frame, altered)
	if mismatch == nil {
		t.Fatal("expected pixel mismatch")
	}
	if mismatch.X != x || mismatch.Y != y || mismatch.ChunkIndex != 4 {
		t.Fatalf("mismatch location = %+v", mismatch)
	}
}

func TestTruthSetVerifiesEveryAnalyticSample(t *testing.T) {
	bundle := testBundle(t)
	mismatches, err := VerifyTruth(bundle.Loaded[SchemaV2], bundle.Truth)
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 0 {
		t.Fatalf("truth mismatches: %+v", mismatches)
	}
	samples := 0
	for _, frame := range bundle.Truth.Frames {
		samples += len(frame.SampleWeights)
	}
	if samples != 18 {
		t.Fatalf("truth samples = %d, want 18", samples)
	}
}
