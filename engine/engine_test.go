package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sampleDataset(t *testing.T) (string, *Scene, *TruthDocument) {
	t.Helper()
	dir := t.TempDir()
	if _, err := GenerateSample(dir); err != nil {
		t.Fatal(err)
	}
	scene, err := LoadSceneFile(filepath.Join(dir, "scene_v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	truth, err := LoadTruthFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	return dir, scene, truth
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func marshalMap(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLoaderThreeVersionsAndValidationErrors(t *testing.T) {
	dir, canonical, _ := sampleDataset(t)
	var loaded []*Scene
	for _, file := range []string{"scene_v1.json", "scene_v2.json", "scene_v3.json"} {
		s, err := LoadSceneFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		loaded = append(loaded, s)
		if err := SemanticDiff(canonical, s); err != nil {
			t.Fatalf("semantic mismatch %s: %v", file, err)
		}
	}
	if loaded[0].SourceVersion != Version1 || loaded[1].SourceVersion != Version2 || loaded[2].SourceVersion != Version3 {
		t.Fatalf("source versions not preserved: %v", []Version{loaded[0].SourceVersion, loaded[1].SourceVersion, loaded[2].SourceVersion})
	}

	v1Path := filepath.Join(dir, "scene_v1.json")
	base := readJSONMap(t, v1Path)
	base["schemaVersion"] = "9.0"
	if _, err := LoadScene(marshalMap(t, base)); ErrorCode(err) != CodeVer {
		t.Fatalf("unknown version expected E_VER, got %v", err)
	}
	base = readJSONMap(t, v1Path)
	delete(base, "schemaVersion")
	if _, err := LoadScene(marshalMap(t, base)); ErrorCode(err) != CodeSchema {
		t.Fatalf("missing version expected E_SCHEMA, got %v", err)
	}

	base = readJSONMap(t, v1Path)
	paths := base["paths"].([]any)
	delete(paths[0].(map[string]any), "keyframes")
	if _, err := LoadScene(marshalMap(t, base)); ErrorCode(err) != CodeSchema || !strings.Contains(err.Error(), "keyframes") {
		t.Fatalf("missing keyframes expected E_SCHEMA with path, got %v", err)
	}

	base = readJSONMap(t, v1Path)
	paths = base["paths"].([]any)
	keys := paths[0].(map[string]any)["keyframes"].([]any)
	keys[1].(map[string]any)["timeMs"] = float64(0)
	if _, err := LoadScene(marshalMap(t, base)); ErrorCode(err) != CodeSelf {
		t.Fatalf("non-monotonic keyframe expected E_SELF, got %v", err)
	}

	base = readJSONMap(t, v1Path)
	paths = base["paths"].([]any)
	segs := paths[0].(map[string]any)["keyframes"].([]any)[0].(map[string]any)["segments"].([]any)
	start := segs[1].(map[string]any)["start"].(map[string]any)
	start["x"] = start["x"].(float64) + 1
	if _, err := LoadScene(marshalMap(t, base)); ErrorCode(err) != CodeSelf {
		t.Fatalf("discontinuous segment expected E_SELF, got %v", err)
	}

	base = readJSONMap(t, v1Path)
	paths = base["paths"].([]any)
	segs = paths[0].(map[string]any)["keyframes"].([]any)[0].(map[string]any)["segments"].([]any)
	end := segs[len(segs)-1].(map[string]any)["end"].(map[string]any)
	end["x"] = end["x"].(float64) + 1
	if _, err := LoadScene(marshalMap(t, base)); ErrorCode(err) != CodeSelf {
		t.Fatalf("open contour expected E_SELF, got %v", err)
	}

	base = readJSONMap(t, v1Path)
	paths = base["paths"].([]any)
	paths[0].(map[string]any)["blend"] = "replace"
	if _, err := LoadScene(marshalMap(t, base)); ErrorCode(err) != CodeComp {
		t.Fatalf("v1 replace blend expected E_COMP, got %v", err)
	}

	base = readJSONMap(t, v1Path)
	paths = base["paths"].([]any)
	paths[0].(map[string]any)["fillColor"] = []any{1, 2, 3}
	if _, err := LoadScene(marshalMap(t, base)); ErrorCode(err) != CodeComp || !strings.Contains(err.Error(), "ember") {
		t.Fatalf("channel count expected E_COMP with path id, got %v", err)
	}

	base = readJSONMap(t, v1Path)
	paths = base["paths"].([]any)
	paths[0].(map[string]any)["closed"] = false
	if _, err := LoadScene(marshalMap(t, base)); ErrorCode(err) != CodeSelf {
		t.Fatalf("closed=false expected E_SELF, got %v", err)
	}
}

func TestIntegerTimeAndCoordinateRoundTrip(t *testing.T) {
	for _, ms := range []int64{0, 250, 500, 750, 1000} {
		ticks := millisToTicks(ms)
		for _, version := range []Version{Version1, Version2, Version3} {
			got, err := RoundTripTime(ticks, version, RoundFloor)
			if err != nil {
				t.Fatalf("time %d version %s: %v", ms, version, err)
			}
			if got != ticks {
				t.Fatalf("time roundtrip changed %d to %d", ticks, got)
			}
		}
	}
	if _, err := RoundTripTime(millisToTicks(125), Version2, RoundFloor); ErrorCode(err) != CodeRT {
		t.Fatalf("non-representable frame time expected E_RT, got %v", err)
	}
	base := Vec2{X: -12345, Y: 998877}
	absolute := Vec2{X: 23456, Y: 776655}
	if err := RoundTripCoordDelta(base, absolute); err != nil {
		t.Fatal(err)
	}
	if err := RoundTripCoordDelta(Vec2{}, absolute); err != nil {
		t.Fatal(err)
	}
}

func TestInterpolationFlattenMonotonicAndTruthTolerance(t *testing.T) {
	_, scene, truth := sampleDataset(t)
	tolerances := []int64{1024, 512, 256, 128, 64, 32, 16, 8, 4, 2, 1}
	var previous int
	for _, tol := range tolerances {
		lines, err := FlattenAt(ApplyQuality(scene, QualityDecision{Tolerance: tol}), scene.Samples[2])
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, l := range lines {
			count += l.SegmentCount
		}
		if count < previous {
			t.Fatalf("line count decreased when tolerance tightened: tol=%d count=%d previous=%d", tol, count, previous)
		}
		previous = count
	}
	fine, err := Run(context.Background(), scene, RunOptions{Mode: ModePreview, MaxWorkers: 2, BudgetBytes: 512 * 1024 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyFlattenAgainstTruth(ApplyQuality(scene, fine.Quality), truth, fine.Frames); err != nil {
		t.Fatal(err)
	}
	bad := scene.Clone()
	bad.Interpolation.Mode = "warp"
	if _, err := FlattenAt(bad, 0); ErrorCode(err) != CodePara {
		t.Fatalf("invalid interpolation expected E_PARA, got %v", err)
	}
	bad = scene.Clone()
	bad.Tolerance = 0
	if _, err := FlattenAt(bad, 0); ErrorCode(err) != CodePara {
		t.Fatalf("invalid tolerance expected E_PARA, got %v", err)
	}
}

func TestRenderingTruthQualityAndCompositeErrors(t *testing.T) {
	_, scene, truth := sampleDataset(t)
	fineQuality, err := SelectQuality(scene, 512*1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	if fineQuality.Mode != "fine" {
		t.Fatalf("large budget selected %s", fineQuality.Mode)
	}
	approxQuality, err := SelectQuality(scene, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if approxQuality.Mode != "approx" || approxQuality.Downsample < 2 {
		t.Fatalf("small budget did not select approximate mode: %+v", approxQuality)
	}
	fineScene := ApplyQuality(scene, fineQuality)
	approxScene := ApplyQuality(scene, approxQuality)
	var frames []*RenderedFrame
	for i, tm := range scene.Samples {
		f, err := RenderFrame(fineScene, tm, i, fineQuality)
		if err != nil {
			t.Fatal(err)
		}
		a, err := RenderFrame(approxScene, tm, i, approxQuality)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckScaleMetric(CompareQualityMetrics(f, a)); err != nil {
			t.Fatal(err)
		}
		wantW := int(max64(1, PixelLength(f.RenderBounds.Width(), fineScene.Scale)))
		wantH := int(max64(1, PixelLength(f.RenderBounds.Height(), fineScene.Scale)))
		if f.Width != wantW || f.Height != wantH {
			t.Fatalf("bitmap dimension not derived from bounds: got %dx%d want %dx%d", f.Width, f.Height, wantW, wantH)
		}
		if len(f.RGBA) != f.Width*f.Height*4 {
			t.Fatalf("RGBA channel count mismatch")
		}
		pngData, err := f.PNG()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := png.Decode(bytes.NewReader(pngData))
		if err != nil {
			t.Fatal(err)
		}
		for px := 0; px < f.Width*f.Height; px++ {
			if f.RGBA[px*4+3] < 250 {
				continue
			}
			c := color.NRGBAModel.Convert(decoded.At(px%f.Width, px/f.Width)).(color.NRGBA)
			if c.R != f.RGBA[px*4] || c.G != f.RGBA[px*4+1] || c.B != f.RGBA[px*4+2] || c.A != f.RGBA[px*4+3] {
				t.Fatalf("PNG color changed at pixel %d: got %+v", px, c)
			}
			break
		}
		frames = append(frames, f)
	}
	if err := VerifyTruth(fineScene, truth, frames, DefaultTruthTolerance()); err != nil {
		t.Fatal(err)
	}
	bad := scene.Clone()
	bad.Paths[0].Blend = "dissolve"
	if _, err := RenderFrame(bad, 0, 0, fineQuality); ErrorCode(err) != CodeComp || !strings.Contains(err.Error(), bad.Paths[0].ID) {
		t.Fatalf("invalid blend expected E_COMP with path id, got %v", err)
	}
	huge := ScaleMetric{Frame: 2, AreaRelativeError: 0.4, BBoxIoU: 0.1}
	if err := CheckScaleMetric(huge); ErrorCode(err) != CodeScale || !strings.Contains(err.Error(), "frame=3") {
		t.Fatalf("bad scale metric expected E_SCALE with frame, got %v", err)
	}
}

func TestStateMachineTransitions(t *testing.T) {
	_, scene, _ := sampleDataset(t)
	m := NewStageMachine(scene)
	tm := scene.Samples[0]
	pid := scene.Paths[0].ID
	if err := m.Transition(tm, pid, StageFlatten, StateCommitted); ErrorCode(err) != CodeState {
		t.Fatalf("pending->committed should be E_STATE, got %v", err)
	}
	if got := m.StateOf(tm, pid, StageFlatten); got != StatePending {
		t.Fatalf("illegal transition changed state to %s", got)
	}
	if err := m.Transition(tm, pid, StageFlatten, StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(tm, pid, StageFlatten, StateCommitted); err != nil {
		t.Fatal(err)
	}
	if err := m.Transition(tm, pid, StageFlatten, StateProcessing); ErrorCode(err) != CodeState {
		t.Fatalf("committed terminal transition should be E_STATE, got %v", err)
	}
}

func TestExportWarningsRoundTripAndEquality(t *testing.T) {
	_, scene, _ := sampleDataset(t)
	for _, target := range []Version{Version1, Version2, Version3} {
		back, _, err := RoundTripScene(scene, target, RoundFloor)
		if err != nil {
			t.Fatalf("roundtrip through %s: %v", target, err)
		}
		if err := SemanticDiff(scene, back); err != nil {
			t.Fatalf("roundtrip semantic mismatch through %s: %v", target, err)
		}
	}
	lossy := scene.Clone()
	lossy.Paths[0].Blend = BlendReplace
	res, err := ExportScene(lossy, Version1, RoundFloor)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Warnings {
		if w.Code == CodeLossy && strings.Contains(w.Path, ".blend") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected E_LOSSY warning for replace blend")
	}
	if _, err := VerifyExport(lossy, res); err != nil {
		t.Fatalf("lossy export should be loadable with documented fallback: %v", err)
	}
	if _, _, err := RoundTripScene(lossy, Version1, RoundFloor); err != nil {
		t.Fatalf("geometry/time roundtrip must not fail on documented blend downgrade: %v", err)
	}
	changed := scene.Clone()
	changed.Paths[0].Fill.R++
	if _, err := VerifyExport(changed, res); ErrorCode(err) != CodeEq || !strings.Contains(err.Error(), "paths[0]") {
		t.Fatalf("expected E_EQ with field path, got %v", err)
	}
}

func TestSnapshotResumeAndHashInvalidation(t *testing.T) {
	_, scene, _ := sampleDataset(t)
	workDir := t.TempDir()
	dbPath := filepath.Join(workDir, "snapshot.db")
	q, err := SelectQuality(scene, 512*1024*1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	full, err := Run(context.Background(), scene, RunOptions{
		Mode: ModeFull, DBPath: dbPath, MaxWorkers: 2, BufferBytes: 4 * 1024 * 1024, Quality: &q,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Frames) != len(scene.Samples) {
		t.Fatalf("full run did not commit all frames")
	}
	resumed, err := Run(context.Background(), scene, RunOptions{
		Mode: ModeResume, DBPath: dbPath, MaxWorkers: 2, BufferBytes: 4 * 1024 * 1024, Quality: &q,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.SnapshotStatus.Valid || resumed.SnapshotStatus.FrameCount != len(scene.Samples) {
		t.Fatalf("resume did not validate complete snapshot: %+v", resumed.SnapshotStatus)
	}
	if diff, err := CompareRuns(full, resumed); err != nil || !diff.Consistent {
		t.Fatalf("full/resume mismatch: %v %+v", err, diff)
	}

	interruptedDB := filepath.Join(workDir, "interrupted.db")
	interrupted, err := Run(context.Background(), scene, RunOptions{
		Mode: ModeResume, DBPath: interruptedDB, MaxWorkers: 2, BufferBytes: 4 * 1024 * 1024,
		Quality: &q, StopAfter: 2,
	})
	if !errors.Is(err, context.Canceled) || !interrupted.Partial || len(interrupted.Frames) != 2 {
		t.Fatalf("expected a 2-frame interrupted prefix, got frames=%d err=%v partial=%v", len(interrupted.Frames), err, interrupted.Partial)
	}
	continued, err := Run(context.Background(), scene, RunOptions{
		Mode: ModeResume, DBPath: interruptedDB, MaxWorkers: 2, BufferBytes: 4 * 1024 * 1024, Quality: &q,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(continued.Frames) != len(scene.Samples) || continued.SnapshotStatus.ResumedFrom != 2 || continued.SnapshotStatus.FrameCount != len(scene.Samples) {
		t.Fatalf("restart did not resume the missing suffix: %+v", continued.SnapshotStatus)
	}
	if diff, err := CompareRuns(full, continued); err != nil || !diff.Consistent {
		t.Fatalf("interrupted resume mismatch: %v %+v", err, diff)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE frames SET content_hash='corrupt' WHERE scene_id=? AND time_us=(SELECT MIN(time_us) FROM frames WHERE scene_id=?)`, scene.ID, scene.ID); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	store, err := OpenSnapshotStore(dbPath, ApplyQuality(scene, q))
	if err != nil {
		t.Fatal(err)
	}
	frames, status, err := store.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Invalidated || status.Valid || len(frames) != 0 {
		t.Fatalf("corrupt snapshot should invalidate entirely: %+v", status)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM frames WHERE scene_id=?`, scene.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalidation left %d rows", count)
	}
	_ = store.Close()

	if _, err := Run(context.Background(), scene, RunOptions{
		Mode: ModeFull, DBPath: dbPath, MaxWorkers: 2, BufferBytes: 4 * 1024 * 1024, Quality: &q,
	}); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE frames SET blend_version='stale-blend' WHERE scene_id=? AND time_us=(SELECT MAX(time_us) FROM frames WHERE scene_id=?)`, scene.ID, scene.ID); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	store, err = OpenSnapshotStore(dbPath, ApplyQuality(scene, q))
	if err != nil {
		t.Fatal(err)
	}
	frames, status, err = store.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Invalidated || status.Valid || len(frames) != 0 {
		t.Fatalf("stale version should invalidate entirely: %+v", status)
	}
	_ = store.Close()
}

func TestThreePathsCommitStabilityAndDiffLocation(t *testing.T) {
	_, scene, _ := sampleDataset(t)
	dbPath := filepath.Join(t.TempDir(), "runs.db")
	q, err := SelectQuality(scene, 512*1024*1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := Run(context.Background(), scene, RunOptions{
		Mode: ModePreview, MaxWorkers: 3, BufferBytes: 4 * 1024 * 1024, Quality: &q,
	})
	if err != nil {
		t.Fatal(err)
	}
	full, err := Run(context.Background(), scene, RunOptions{
		Mode: ModeFull, DBPath: dbPath, MaxWorkers: 3, BufferBytes: 4 * 1024 * 1024, Quality: &q,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := Run(context.Background(), scene, RunOptions{
		Mode: ModeResume, DBPath: dbPath, MaxWorkers: 3, BufferBytes: 4 * 1024 * 1024, Quality: &q,
	})
	if err != nil {
		t.Fatal(err)
	}
	diffReport, err := CompareRuns(preview, full, resumed)
	if err != nil || !diffReport.Consistent {
		t.Fatalf("three paths differ: %v %+v", err, diffReport)
	}
	bad := *full
	badFrames := make([]*RenderedFrame, len(full.Frames))
	copy(badFrames, full.Frames)
	frameCopy := *full.Frames[0]
	frameCopy.RGBA = append([]byte(nil), full.Frames[0].RGBA...)
	frameCopy.RGBA[0] ^= 0xff
	frameCopy.ContentHash = "intentional-diff"
	badFrames[0] = &frameCopy
	bad.Frames = badFrames
	report, err := CompareRuns(preview, &bad)
	if ErrorCode(err) != CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Pixel == nil || e.Pixel.X != 0 || e.Pixel.Y != 0 {
		t.Fatalf("E_DIFF did not locate first pixel: %+v", e)
	}
	if report.Field != "bitmap.rgba" {
		t.Fatalf("unexpected difference field %q", report.Field)
	}
}

func TestBufferExhaustionRecordsBlockingReason(t *testing.T) {
	_, scene, _ := sampleDataset(t)
	q, err := SelectQuality(scene, 512*1024*1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), scene, RunOptions{
		Mode: ModeFull, DBPath: filepath.Join(t.TempDir(), "buffer.db"), MaxWorkers: 2, BufferBytes: 1, Quality: &q,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.BufferEvents) == 0 {
		t.Fatal("expected buffer exhaustion events")
	}
	if report.BufferEvents[0].Reason == "" {
		t.Fatal("buffer event missing reason")
	}
}

func TestLossyJSONWarningShapeAndSampleManifest(t *testing.T) {
	dir, scene, _ := sampleDataset(t)
	manifest, err := GenerateSample(dir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SceneID != scene.ID || !reflect.DeepEqual(manifest.SampleTimesMs, []int64{0, 250, 500, 750, 1000}) {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	data, err := json.Marshal(manifest)
	if err != nil || !strings.Contains(string(data), "truth.json") {
		t.Fatalf("manifest marshal failure: %v", err)
	}
}
