package morph

import (
	"context"
	"math"
	"path/filepath"
	"testing"
)

func TestSmokeGenerateRun(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID = "smoke"
	opts.Paths = 4
	opts.Keyframes = 3
	opts.Segments = 6
	opts.Samples = 7
	opts.Scale = 8
	opts.ReferenceGrid = 48
	scene, truth, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfigForScene(scene)
	cfg.Checkpoint = t.TempDir()
	cfg.Runtime.Workers = 2
	cfg.Render.Grid = 2
	cfg.Adaptive.ForceFine = true
	engine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := engine.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("run: %v summary=%+v", err, summary)
	}
	if summary.CommittedSamples != len(scene.SampleTimes) {
		t.Fatalf("committed=%d", summary.CommittedSamples)
	}
	results := engine.Results()
	rep, err := ValidateResultsAgainstTruth(scene, truth, results, cfg, 1e-6, 0.2)
	if err != nil {
		for i := range results[0].PathStats {
			t.Logf("path=%s got=%.6f truth=%.6f stroke=%v fill=%v", results[0].PathStats[i].PathID, results[0].PathStats[i].CoverageArea, truth.Samples[0].Paths[i].ReferenceCoverageArea, scene.Paths[i].Stroke != nil, scene.Paths[i].Fill != nil)
		}
		t.Fatalf("truth: %v report=%+v", err, rep)
	}
	t.Logf("width=%d height=%d segments=%d", results[0].Width, results[0].Height, sumSegments(results[0]))
}

func TestQualityFineApprox(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID = "quality"
	opts.Paths = 6
	opts.Keyframes = 3
	opts.Segments = 6
	opts.Samples = 9
	opts.Scale = 8
	opts.ReferenceGrid = 64
	scene, truth, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	base := DefaultConfigForScene(scene)
	base.Runtime.Workers = 2
	base.Render.Grid = 4
	base.Adaptive.BaseGrid = 4
	base.Adaptive.ForceFine = true
	base.Checkpoint = t.TempDir()
	fineEngine, err := NewEngine(scene, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fineEngine.Run(context.Background(), RunOptions{}); err != nil {
		t.Fatal(err)
	}

	approx := base
	approx.Checkpoint = t.TempDir()
	approx.Adaptive.ForceFine = false
	approx.Adaptive.WorkThreshold = 1
	approx.Adaptive.MemoryBudgetBytes = 1 << 30
	approxEngine, err := NewEngine(scene, approx)
	if err != nil {
		t.Fatal(err)
	}
	if approxEngine.Quality.Mode != "approx" {
		t.Fatalf("mode=%s", approxEngine.Quality.Mode)
	}
	if _, err := approxEngine.Run(context.Background(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	report, err := CompareQuality(scene, truth, base.Consistency, fineEngine.Results(), approxEngine.Results())
	if err != nil {
		t.Fatalf("quality: %v\nreport=%+v", err, report)
	}
	t.Logf("segments fine=%d approx=%d midpoint=%v mae=%.6f", report.FineSegments, report.ApproxSegments, report.MidpointCovered, report.Samples[0].PixelError.MAE)
}

func TestThreePathsExact(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID = "paths3"
	opts.Paths = 4
	opts.Keyframes = 3
	opts.Segments = 6
	opts.Samples = 9
	opts.Scale = 6
	opts.ReferenceGrid = 32
	scene, _, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	base := DefaultConfigForScene(scene)
	base.Runtime.Workers = 2
	base.Render.Grid = 3
	base.Adaptive.BaseGrid = 3
	base.Adaptive.ForceFine = true

	run := func(cp string, stop int) []*SampleResult {
		cfg := base
		cfg.Checkpoint = cp
		e, err := NewEngine(scene, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Run(context.Background(), RunOptions{StopAfterCommits: stop}); err != nil {
			t.Fatal(err)
		}
		if stop > 0 {
			e2, err := NewEngine(scene, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e2.Run(context.Background(), RunOptions{}); err != nil {
				t.Fatal(err)
			}
			return e2.Results()
		}
		return e.Results()
	}
	full := run(t.TempDir(), 0)
	resume := run(t.TempDir(), 4)
	incremental := run(t.TempDir(), 7)
	rep, err := CompareResultFrames(scene, []string{"full", "resume", "incremental"}, [][]*SampleResult{full, resume, incremental})
	if err != nil {
		t.Fatalf("compare: %v\nrep=%+v", err, rep)
	}
	if !rep.Pass {
		t.Fatal("not identical")
	}
}

func TestFailureRetryState(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID, opts.Paths, opts.Keyframes, opts.Segments, opts.Samples = "retry", 3, 2, 5, 5
	opts.Scale = 6
	scene, _, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfigForScene(scene)
	cfg.Checkpoint = t.TempDir()
	cfg.Runtime.Workers = 1
	cfg.Adaptive.ForceFine = true
	e, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	faults := FaultPlan{0: {scene.Paths[1].ID: StageCoverage}}
	_, err = e.Run(context.Background(), RunOptions{Faults: faults})
	if err == nil || CodeOf(err) != CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if st := e.Timeline.State(0, scene.Paths[1].ID, StageCoverage); st != StateFailed {
		t.Fatalf("state=%s", st)
	}
	if _, err := e.RetrySample(context.Background(), 0, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if e.Timeline.CommittedSamples() != 1 {
		t.Fatalf("committed=%d", e.Timeline.CommittedSamples())
	}
	if _, err := e.Run(context.Background(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if e.Timeline.CommittedSamples() != len(scene.SampleTimes) {
		t.Fatal("not complete")
	}
}

func TestStateMachineIllegalPreserves(t *testing.T) {
	scene := &Scene{Paths: []Path{{ID: "p"}}, SampleTimes: []float64{0}}
	tl := NewTimeline(scene)
	if tl.State(0, "p", StageFlatten) != StatePending {
		t.Fatal("initial")
	}
	if err := tl.Transition(0, "p", StageFlatten, StateCommitted); CodeOf(err) != CodeState {
		t.Fatalf("err=%v", err)
	}
	if tl.State(0, "p", StageFlatten) != StatePending {
		t.Fatal("state mutated")
	}
	if err := tl.Transition(0, "p", StageFlatten, StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := tl.Transition(0, "p", StageFlatten, StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := tl.Transition(0, "p", StageFlatten, StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := tl.Transition(0, "p", StageFlatten, StateCommitted); err != nil {
		t.Fatal(err)
	}
	if err := tl.Transition(0, "p", StageFlatten, StatePending); CodeOf(err) != CodeState {
		t.Fatal("committed mutated")
	}
}

func TestRasterSquare(t *testing.T) {
	rect := PixelRect{0, 0, 20, 20}
	for _, g := range []int{2, 4} {
		m := make([]float32, rect.Area())
		rasterPolygon(m, rect, []Vec2{{5, 5}, {15, 5}, {15, 15}, {5, 15}}, g, "nonzero")
		sum := 0.0
		for _, v := range m {
			sum += float64(v)
		}
		if math.Abs(sum-100) > 1e-5 {
			t.Fatalf("grid=%d area=%.6f", g, sum)
		}
	}
}

func TestFlattenMonotonicAndCurveTruth(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID, opts.Paths, opts.Keyframes, opts.Segments, opts.Samples = "flatten", 2, 2, 6, 3
	scene, _, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	_, interpCount, err := InterpolatePath(&scene.Paths[0], scene.SampleTimes[1], "linear")
	if err != nil || interpCount == 0 {
		t.Fatalf("interp=%d err=%v", interpCount, err)
	}
	tols := []float64{0.2, 0.1, 0.05, 0.02, 0.01, 0.005}
	prev := -1
	for _, tol := range tols {
		flat, err := FlattenPath(&scene.Paths[0], scene.SampleTimes[1], EvalConfig{Interpolation: "linear", Scale: 1, MidTolerance: tol})
		if err != nil {
			t.Fatal(err)
		}
		if flat.SegmentCount < prev {
			t.Fatalf("non-monotone: tol=%g count=%d prev=%d", tol, flat.SegmentCount, prev)
		}
		prev = flat.SegmentCount
	}
	p := &scene.Paths[0]
	const sampleT = 0.25
	segs, _, err := InterpolatePath(p, sampleT, "linear")
	if err != nil {
		t.Fatal(err)
	}
	u := (sampleT - p.Keyframes[0].Time) / (p.Keyframes[1].Time - p.Keyframes[0].Time)
	wantP1 := lerpVec(p.Keyframes[0].Segments[0].P1, p.Keyframes[1].Segments[0].P1, u)
	if segs[0].P1.Distance(wantP1) > 1e-12 {
		t.Fatalf("interpolated control point mismatch: got=%+v want=%+v", segs[0].P1, wantP1)
	}
}

func TestCommittedFrozenAcrossParamChange(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID, opts.Paths, opts.Keyframes, opts.Segments, opts.Samples = "freeze", 3, 2, 5, 7
	opts.Scale = 6
	scene, _, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	cp := t.TempDir()
	cfg := DefaultConfigForScene(scene)
	cfg.Checkpoint = cp
	cfg.Runtime.Workers = 1
	cfg.Adaptive.ForceFine = true
	cfg.Render.Grid = 4
	cfg.Adaptive.BaseGrid = 4
	e, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), RunOptions{StopAfterCommits: 3}); err != nil {
		t.Fatal(err)
	}
	before := e.Results()
	prefix := make([]string, 3)
	for i := 0; i < 3; i++ {
		prefix[i] = before[i].PixelHash
	}

	cfg2 := cfg
	cfg2.Eval.MidTolerance = 0.08
	cfg2.Render.Grid = 2
	cfg2.Adaptive.BaseGrid = 2
	e2, err := NewEngine(scene, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e2.Run(context.Background(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	after := e2.Results()
	for i := 0; i < 3; i++ {
		if after[i].PixelHash != prefix[i] {
			t.Fatalf("committed frame %d was rewritten", i)
		}
		if !after[i].FromCheckpoint {
			t.Fatalf("frame %d did not come from checkpoint", i)
		}
	}
}

func TestBufferWaitRecorded(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID, opts.Paths, opts.Keyframes, opts.Segments, opts.Samples = "buffer", 4, 2, 5, 6
	opts.Scale = 6
	scene, _, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfigForScene(scene)
	cfg.Checkpoint = t.TempDir()
	cfg.Runtime.Workers = 3
	cfg.Adaptive.MemoryBudgetBytes = 10 << 20
	cfg.Adaptive.ForceFine = true
	cfg.Render.Grid = 2
	cfg.Adaptive.BaseGrid = 2
	e, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := e.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("run: %v sum=%+v", err, sum)
	}
	if len(sum.ResourceEvents) == 0 {
		t.Fatalf("expected pixel buffer wait record: %+v", sum)
	}
}

func TestCompressedSceneRoundTrip(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID, opts.Paths, opts.Keyframes, opts.Segments, opts.Samples = "gzip", 2, 2, 4, 3
	scene, truth, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	sp, tp, err := WriteGeneratedCompressed(t.TempDir(), scene, truth)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(sp) != ".gz" || filepath.Ext(tp) != ".gz" {
		t.Fatal("not compressed")
	}
	gotScene, err := LoadScene(sp)
	if err != nil {
		t.Fatal(err)
	}
	gotTruth, err := LoadTruth(tp)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateScene(gotScene); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTruth(gotScene, gotTruth); err != nil {
		t.Fatal(err)
	}
}

func TestStopAndContinueSameEngine(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID, opts.Paths, opts.Keyframes, opts.Segments, opts.Samples = "continue", 2, 2, 4, 6
	opts.Scale = 6
	scene, _, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfigForScene(scene)
	cfg.Checkpoint = t.TempDir()
	cfg.Runtime.Workers = 2
	cfg.Adaptive.ForceFine = true
	e, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), RunOptions{StopAfterCommits: 2}); err != nil {
		t.Fatal(err)
	}
	sum, err := e.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.CommittedSamples != len(scene.SampleTimes) {
		t.Fatalf("committed=%d", sum.CommittedSamples)
	}
}
