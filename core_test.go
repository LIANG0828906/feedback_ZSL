package vectorcore

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func testContour(dx, dy int64) Contour {
	px := func(v int64) int64 { return v * CoordScale }
	left, right := px(30)+dx, px(110)+dx
	top, bottom := px(20)+dy, px(90)+dy
	start := Point{X: left, Y: px(55) + dy}
	return Contour{
		Start: start,
		Segments: []Segment{
			{Kind: SegmentQuad, C1: Point{X: left, Y: top}, To: Point{X: px(70) + dx, Y: top}},
			{Kind: SegmentQuad, C1: Point{X: right, Y: top}, To: Point{X: right, Y: px(55) + dy}},
			{Kind: SegmentCubic, C1: Point{X: right, Y: bottom}, C2: Point{X: px(70) + dx, Y: bottom}, To: Point{X: px(70) + dx, Y: bottom}},
			{Kind: SegmentCubic, C1: Point{X: px(30) + dx, Y: bottom}, C2: Point{X: left, Y: px(70) + dy}, To: start},
		},
	}
}

func testScene() *Scene {
	fps := int64(60)
	scene := &Scene{ID: "test-scene", SourceVersion: 3, FPS: fps, Rounding: RoundNearest}
	for _, frame := range []int64{0, 3, 6, 9, 12} {
		t, _ := FrameToTicks(frame, fps, RoundNearest)
		scene.SampleTimes = append(scene.SampleTimes, t)
	}
	fill := Path{
		ID: "fill", Paint: Paint{Fill: true, Color: RGBA{R: 40, G: 180, B: 240, A: 220},
			Opacity: 9000, Blend: BlendNormal, Join: JoinRound},
	}
	stroke := Path{
		ID: "stroke", Paint: Paint{Fill: false, StrokeWidth: 5 * CoordScale, Join: JoinRound,
			Color: RGBA{R: 255, G: 160, B: 50, A: 240}, Opacity: 8500, Blend: BlendSourceOver},
	}
	for ki, frame := range []int64{0, 6, 12} {
		t, _ := FrameToTicks(frame, fps, RoundNearest)
		drift := int64(ki-1) * 4 * CoordScale
		fill.Keyframes = append(fill.Keyframes, Keyframe{Time: t, Contour: testContour(0, drift)})
		stroke.Keyframes = append(stroke.Keyframes, Keyframe{Time: t, Contour: testContour(12*CoordScale, -drift)})
	}
	scene.Paths = []Path{fill, stroke}
	return scene
}

func TestLoadThreeVersions(t *testing.T) {
	base := testScene()
	var loaded []*Scene
	for version := 1; version <= 3; version++ {
		data, err := EncodeScene(base, version)
		if err != nil {
			t.Fatalf("encode v%d: %v", version, err)
		}
		scene, err := LoadSceneBytes(data)
		if err != nil {
			t.Fatalf("load v%d: %v", version, err)
		}
		if scene.SourceVersion != version {
			t.Fatalf("source version = %d, want %d", scene.SourceVersion, version)
		}
		loaded = append(loaded, scene)
	}
	for i := 1; i < len(loaded); i++ {
		a, b := loaded[0].Clone(), loaded[i].Clone()
		a.SourceVersion, b.SourceVersion = 0, 0
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("v1 and v%d canonical scenes differ", i+1)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	_, err := LoadSceneBytes([]byte(`{"schemaVersion":9}`))
	if CodeOf(err) != CodeVersion {
		t.Fatalf("unknown version error = %v, code=%s", err, CodeOf(err))
	}
	_, err = LoadSceneBytes([]byte(`{"schemaVersion":3,"timeline":{"unit":"frame","fps":60,"samples":[0]},"paths":[]}`))
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeSchema || e.Field != "sceneId" || e.SourceVersion != 3 {
		t.Fatalf("missing field error = %#v", err)
	}
	_, err = LoadSceneBytes([]byte(`{"schemaVersion":`))
	if CodeOf(err) != CodeDecode {
		t.Fatalf("malformed scene code = %s (%v)", CodeOf(err), err)
	}
	_, err = LoadSceneBytes([]byte(`{"schemaVersion":3,"sceneId":"x","timeline":{"unit":"frame","fps":60,"samples":[9223372036854775807]},"paths":[]}`))
	if CodeOf(err) != CodeOverflow {
		t.Fatalf("overflow code = %s (%v)", CodeOf(err), err)
	}
	_, err = LoadSceneBytes([]byte(`{
		"schema_version":2,"id":"x",
		"timeline":{"unit":"ms","fps":60,"samples":[0]},
		"paths":[{
			"path_id":"p",
			"paint":{"fill":true,"color":[1,2,3,255],"opacity":1000},
			"tracks":[{"at":0,"shape":{"base":{"x":9223372036854775807,"y":0},
				"segments":[{"kind":"quad","c1":{"dx":1,"dy":0},"to":{"dx":0,"dy":0}}]}}]
		}]
	}`))
	if !errors.As(err, &e) || e.Code != CodeOverflow || e.PathID != "p" || e.Keyframe != 0 ||
		!strings.Contains(e.Field, "segments[0].c1.dx") {
		t.Fatalf("relative coordinate overflow error = %#v", err)
	}
	if _, err := FrameToTicks(math.MaxInt64, 60, RoundNearest); CodeOf(err) != CodeOverflow {
		t.Fatalf("public frame conversion overflow code = %s (%v)", CodeOf(err), err)
	}
}

func TestHighLowHighRoundTrip(t *testing.T) {
	scene := testScene()
	v2, err := EncodeScene(scene, 2)
	if err != nil {
		t.Fatal(err)
	}
	low, err := LoadSceneBytes(v2)
	if err != nil {
		t.Fatal(err)
	}
	v3, err := EncodeScene(low, 3)
	if err != nil {
		t.Fatal(err)
	}
	high, err := LoadSceneBytes(v3)
	if err != nil {
		t.Fatal(err)
	}
	a, b := scene.Clone(), high.Clone()
	a.SourceVersion, b.SourceVersion = 0, 0
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("v3 -> v2 -> v3 round trip changed fields")
	}
	for _, p := range scene.Paths {
		for _, k := range p.Keyframes {
			ms, err := TicksToMillis(k.Time, scene.Rounding)
			if err != nil {
				t.Fatal(err)
			}
			back, err := MillisToTicks(ms, scene.Rounding)
			if err != nil || back != k.Time {
				t.Fatalf("time round trip %d -> %d -> %d: %v", k.Time, ms, back, err)
			}
		}
	}
}

func TestFlattenMonotonic(t *testing.T) {
	contour := testContour(0, 0)
	exact, err := ContourAreaExact(contour)
	if err != nil {
		t.Fatal(err)
	}
	tolerances := []int64{4096, 2048, 1024, 512, 256, 128, 64}
	var prevN int
	var prevErr int64 = math.MaxInt64
	for i, tolerance := range tolerances {
		poly, err := FlattenContour(contour, tolerance)
		if err != nil {
			t.Fatal(err)
		}
		if !poly.Closed || poly.Points[0] != poly.Points[len(poly.Points)-1] {
			t.Fatalf("tolerance %d produced open contour", tolerance)
		}
		area, err := PolygonArea(poly.Points)
		if err != nil {
			t.Fatal(err)
		}
		areaErr := area - exact
		if areaErr < 0 {
			areaErr = -areaErr
		}
		if i > 0 && len(poly.Points) < prevN {
			t.Fatalf("segments decreased from %d to %d at tolerance %d", prevN, len(poly.Points), tolerance)
		}
		if i > 0 && areaErr > prevErr {
			t.Fatalf("area error increased from %d to %d at tolerance %d", prevErr, areaErr, tolerance)
		}
		prevN, prevErr = len(poly.Points), areaErr
	}
	_, err = FlattenContour(contour, 0)
	if CodeOf(err) != CodeParam {
		t.Fatalf("invalid tolerance code = %s", CodeOf(err))
	}
	cfg := DefaultConfig()
	cfg.Scale = Scale{Num: 0, Den: 1}
	if CodeOf(cfg.Validate()) != CodeParam {
		t.Fatalf("invalid scale was accepted")
	}
}

func TestRasterCompositeAndModeErrors(t *testing.T) {
	scene := testScene()
	cfg := DefaultConfig()
	cfg.Grid = 2
	engine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	timeline, err := engine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tr := timeline.Times[0]
	if tr.Bitmap.Width <= 2 || tr.Bitmap.Height <= 2 {
		t.Fatalf("unexpected bitmap size %dx%d", tr.Bitmap.Width, tr.Bitmap.Height)
	}
	nonZero := false
	for _, p := range tr.Bitmap.Pixels {
		if p != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Fatal("rendered bitmap is empty")
	}
	bad := scene.Paths[0].Paint
	bad.Blend = "overlay"
	_, err = CompositePath(tr.Bitmap, tr.Paths[0].Coverage, bad, "fill")
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeMode || e.PathID != "fill" {
		t.Fatalf("invalid blend error = %#v", err)
	}
}

func TestStateMachineAndFreeze(t *testing.T) {
	scene := testScene()
	cfg := DefaultConfig()
	cfg.Grid = 1
	engine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Transition("fill", 0, StageFlatten, StateCommitted); CodeOf(err) != CodeState {
		t.Fatalf("illegal transition code = %s", CodeOf(err))
	}
	if got := engine.StageState("fill", 0, StageFlatten); got != StatePending {
		t.Fatalf("state changed after illegal transition: %s", got)
	}
	if _, err := engine.RenderAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	committed, total := engine.Progress()
	if committed != total || total != len(scene.SampleTimes) {
		t.Fatalf("progress = %d/%d", committed, total)
	}
	if got := engine.StageState("fill", 0, StageComposite); got != StateCommitted {
		t.Fatalf("composite state = %s", got)
	}
	if err := engine.Transition("fill", 0, StageComposite, StatePending); CodeOf(err) != CodeState {
		t.Fatalf("committed transition code = %s", CodeOf(err))
	}
	if got := engine.StageState("fill", 0, StageComposite); got != StateCommitted {
		t.Fatalf("committed state changed: %s", got)
	}
	invs, err := engine.UpdatePaint("fill", func() Paint {
		p := scene.Paths[0].Paint
		p.Opacity = 5000
		return p
	}())
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Fatalf("committed times were invalidated: %+v", invs)
	}
}

func TestPreciseInvalidationAndIncrementalFull(t *testing.T) {
	scene := testScene()
	cfg := DefaultConfig()
	cfg.Grid = 2
	engine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.PrimeCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	newContour := testContour(0, 9*CoordScale)
	invs, err := engine.UpdateKeyframe("fill", 2, newContour)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) == 0 {
		t.Fatal("keyframe edit produced no invalidations")
	}
	for _, inv := range invs {
		if inv.TimeIndex < 3 {
			t.Fatalf("keyframe 2 invalidated earlier time %d: %+v", inv.TimeIndex, inv)
		}
		if !strings.Contains(inv.Reason, "keyframe 2") {
			t.Fatalf("invalidation reason missing keyframe: %+v", inv)
		}
	}
	timeline, err := engine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fullEngine, err := NewEngine(engine.Scene(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullEngine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if diff, err := CompareTimelines(timeline, full); err != nil {
		t.Fatalf("incremental/full diff: %+v (%v)", diff, err)
	}

	preview, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.PrimeCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	paint := scene.Paths[0].Paint
	paint.Opacity = 3210
	if _, err := preview.UpdatePaint("fill", paint); err != nil {
		t.Fatal(err)
	}
	incremental, err := preview.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fullEngine2, err := NewEngine(preview.Scene(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	full2, err := fullEngine2.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if diff, err := CompareTimelines(incremental, full2); err != nil {
		t.Fatalf("opacity incremental/full diff: %+v (%v)", diff, err)
	}
	stats := preview.CacheStats()
	if stats.Hits == 0 {
		t.Fatal("incremental render reused no cache entries")
	}
}

func TestFaultRetryCheckpointResume(t *testing.T) {
	scene := testScene()
	cfg := DefaultConfig()
	cfg.Grid = 1
	cfg.MaxWorkers = 2
	fullEngine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullEngine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dbPath := t.TempDir() + "/checkpoint.db"
	store, err := OpenCheckpoint(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine.AttachCheckpoint(store)
	failed := false
	engine.SetFaultHook(func(timeIndex int, pathID string, stage Stage) error {
		if timeIndex == 2 && stage == StageCoverage && !failed {
			failed = true
			return errors.New("injected coverage fault")
		}
		return nil
	})
	if _, err := engine.RenderAll(context.Background()); err == nil {
		t.Fatal("expected injected failure")
	}
	if committed, _ := engine.Progress(); committed != 2 {
		t.Fatalf("progress after fault = %d, want 2", committed)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenCheckpoint(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resumed, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.LoadCheckpoint(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if committed, _ := resumed.Progress(); committed != 2 {
		t.Fatalf("resume progress = %d, want 2", committed)
	}
	timeline, err := resumed.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if diff, err := CompareTimelines(timeline, full); err != nil {
		t.Fatalf("resume/full diff: %+v (%v)", diff, err)
	}
}

func TestRetryInProcessAndDependencies(t *testing.T) {
	scene := testScene()
	cfg := DefaultConfig()
	cfg.Grid = 1
	fullEngine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullEngine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	engine.SetFaultHook(func(timeIndex int, pathID string, stage Stage) error {
		if timeIndex == 2 && stage == StageCoverage && !failed {
			failed = true
			return errors.New("one-shot fault")
		}
		return nil
	})
	if _, err := engine.RenderAll(context.Background()); err == nil {
		t.Fatal("expected fault")
	}
	if engine.StageState("fill", 2, StageCoverage) != StateFailed {
		t.Fatalf("failed state not recorded: %s", engine.StageState("fill", 2, StageCoverage))
	}
	engine.SetFaultHook(nil)
	if err := engine.RetryTime(2); err != nil {
		t.Fatal(err)
	}
	retried, err := engine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if diff, err := CompareTimelines(retried, full); err != nil {
		t.Fatalf("retry diff: %+v %v", diff, err)
	}
	if committed, _ := engine.Progress(); committed != len(scene.SampleTimes) {
		t.Fatalf("progress after retry = %d", committed)
	}
	if len(engine.Dependencies()) == 0 {
		t.Fatal("cache dependencies are empty")
	}
	hasKeyframeDependency := false
	hasUpstream := false
	for _, dep := range engine.Dependencies() {
		if dep.Stage == StageFlatten && len(dep.Keyframes) > 0 {
			hasKeyframeDependency = true
		}
		if dep.UpstreamKey != "" {
			hasUpstream = true
		}
	}
	if !hasKeyframeDependency || !hasUpstream {
		t.Fatalf("dependency graph lacks keyframe/upstream links")
	}
}

func TestPreviewResumeFullConsistency(t *testing.T) {
	scene := testScene()
	cfg := DefaultConfig()
	cfg.Grid = 1
	cfg.MaxWorkers = 3
	previewEngine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := previewEngine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenCheckpoint(t.TempDir() + "/consistency.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persistedEngine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	persistedEngine.AttachCheckpoint(store)
	persisted, err := persistedEngine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resumeEngine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumeEngine.LoadCheckpoint(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	resumed, err := resumeEngine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fullEngine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullEngine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, timeline := range map[string]*TimelineResult{"preview": preview, "persisted": persisted, "resumed": resumed} {
		if diff, err := CompareTimelines(timeline, full); err != nil {
			t.Fatalf("%s vs full: %+v %v", name, diff, err)
		}
	}
}

func TestCheckpointCorruptionAndLimits(t *testing.T) {
	scene := testScene()
	cfg := DefaultConfig()
	cfg.Grid = 1
	store, err := OpenCheckpoint(t.TempDir() + "/corrupt.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine.AttachCheckpoint(store)
	if _, err := engine.RenderAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE checkpoints SET payload=x'deadbeef' WHERE time_index=1`); err != nil {
		t.Fatal(err)
	}
	resume, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := resume.LoadCheckpoint(context.Background(), store); CodeOf(err) != CodeDecode {
		t.Fatalf("corrupt checkpoint code = %s (%v)", CodeOf(err), err)
	}
	bad := cfg
	bad.MaxWorkers = MaxWorkersHardLimit + 1
	if e := bad.Validate(); CodeOf(e) != CodeParam || e.(*Error).Field != "maxWorkers" {
		t.Fatalf("worker limit validation = %v", e)
	}
	bad = cfg
	bad.MaxPixelBufferBytes = MaxBufferBytesHardLimit + 1
	if e := bad.Validate(); CodeOf(e) != CodeParam || e.(*Error).Field != "maxPixelBufferBytes" {
		t.Fatalf("buffer limit validation = %v", e)
	}
}

func TestInvalidOpacityReportsPath(t *testing.T) {
	scene := testScene()
	scene.Paths[1].Paint.Opacity = 10001
	_, err := NewEngine(scene, DefaultConfig())
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeMode || e.PathID != "stroke" {
		t.Fatalf("invalid opacity error = %#v", err)
	}
}

func TestInterpolationModesAndScale(t *testing.T) {
	scene := testScene()
	cfg := DefaultConfig()
	cfg.Interpolation = InterpHold
	eval, err := EvaluateFrame(scene, 1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if eval.Paths[0].Contour.Start != scene.Paths[0].Keyframes[0].Contour.Start {
		t.Fatal("hold interpolation did not keep the left keyframe")
	}
	cfg.Interpolation = InterpLinear
	linear, err := EvaluateFrame(scene, 1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if linear.Paths[0].Contour.Start == scene.Paths[0].Keyframes[0].Contour.Start {
		t.Fatal("linear interpolation should differ at a non-keyframe time")
	}
	cfg.Interpolation = InterpSmoothStep
	if _, err := EvaluateFrame(scene, 1, cfg); err != nil {
		t.Fatalf("smoothstep: %v", err)
	}
	cfg.Interpolation = "bad"
	if _, err := EvaluateFrame(scene, 1, cfg); CodeOf(err) != CodeParam {
		t.Fatalf("bad interpolation code = %s", CodeOf(err))
	}
	bounds := Bounds{MinX: 0, MinY: 0, MaxX: 100 * CoordScale, MaxY: 50 * CoordScale}
	baseCfg := DefaultConfig()
	vp1, err := DeriveViewport(bounds, baseCfg)
	if err != nil {
		t.Fatal(err)
	}
	baseCfg.Scale = Scale{Num: 2, Den: 1}
	vp2, err := DeriveViewport(bounds, baseCfg)
	if err != nil {
		t.Fatal(err)
	}
	if vp2.Width <= vp1.Width || vp2.Height <= vp1.Height {
		t.Fatalf("scale did not increase viewport: %dx%d -> %dx%d", vp1.Width, vp1.Height, vp2.Width, vp2.Height)
	}
}

func TestDiffLocatesFirstMismatch(t *testing.T) {
	scene := testScene()
	cfg := DefaultConfig()
	cfg.Grid = 1
	engine, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	timeline, err := engine.RenderAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mutated := timeline.Clone()
	mutated.Times[1].Paths[0].Coverage.Values[3] ^= 1
	diff, err := CompareTimelines(mutated, timeline)
	if CodeOf(err) != CodeDiff || diff == nil {
		t.Fatalf("diff code=%s diff=%+v", CodeOf(err), diff)
	}
	if diff.TimeIndex != 1 || diff.PathID != "fill" || diff.Stage != StageCoverage {
		t.Fatalf("unexpected diff location: %+v", diff)
	}
	if diff.X != 3%timeline.Times[1].Viewport.Width || diff.Y != 3/timeline.Times[1].Viewport.Width {
		t.Fatalf("unexpected diff pixel: %+v", diff)
	}
}
