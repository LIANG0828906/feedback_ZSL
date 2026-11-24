package engine_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"virtualstudio/compose"
	"virtualstudio/engine"
	"virtualstudio/formats"
	"virtualstudio/model"
	"virtualstudio/sample"
	"virtualstudio/store"
)

type fixture struct {
	dir      string
	scene    *model.Scene
	commands *model.CommandSet
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 42); err != nil {
		t.Fatal(err)
	}
	scene, err := formats.LoadScene(filepath.Join(dir, "v1", "scene.json"))
	if err != nil {
		t.Fatal(err)
	}
	commands, err := formats.LoadCommands(filepath.Join(dir, "commands.json"), scene)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{dir: dir, scene: scene, commands: commands}
}

func openWS(t *testing.T, f fixture, db, project string) *engine.Workspace {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := engine.OpenWorkspace(context.Background(), engine.WorkspaceOptions{
		Store: st, ProjectID: project, Scene: f.scene, Commands: f.commands, TargetFormat: "v3",
	})
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	return ws
}

func runWS(t *testing.T, ws *engine.Workspace, opts engine.RunOptions) *engine.RunResult {
	t.Helper()
	result, err := ws.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func requireSameRuns(t *testing.T, expected, actual *engine.RunResult) {
	t.Helper()
	mismatch := model.CompareRunSets(
		model.RunFrameSet{Frames: expected.Frames, Commits: expected.CommitSequence},
		model.RunFrameSet{Frames: actual.Frames, Commits: actual.CommitSequence},
		expected.WindowSize,
	)
	if mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestFullRealtimeResumeAndConcurrency(t *testing.T) {
	f := newFixture(t)
	full := openWS(t, f, filepath.Join(f.dir, "full.db"), "full")
	baseline := runWS(t, full, engine.RunOptions{Mode: "full", MaxWorkers: 3, ForceFull: true, MaxRetries: 1})
	_ = full.Store().Close()
	if baseline.Progress.Committed != 6 || baseline.Progress.Total != 6 {
		t.Fatalf("progress=%+v", baseline.Progress)
	}
	if fmt.Sprint(baseline.CommitSequence) != "[0 1 2 3 4 5]" {
		t.Fatalf("commit sequence=%v", baseline.CommitSequence)
	}
	expectedFrames, err := compose.RenderAll(f.scene, f.commands, compose.RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mismatch := model.CompareRunSets(
		model.RunFrameSet{Frames: expectedFrames, Commits: baseline.CommitSequence},
		model.RunFrameSet{Frames: baseline.Frames, Commits: baseline.CommitSequence}, 4)
	if mismatch != nil {
		t.Fatal(mismatch)
	}

	oneWorker := openWS(t, f, filepath.Join(f.dir, "w1.db"), "w1")
	r1 := runWS(t, oneWorker, engine.RunOptions{Mode: "realtime", MaxWorkers: 1, MaxRetries: 1})
	_ = oneWorker.Store().Close()
	fourWorkers := openWS(t, f, filepath.Join(f.dir, "w4.db"), "w4")
	r4 := runWS(t, fourWorkers, engine.RunOptions{Mode: "realtime", MaxWorkers: 4, MaxRetries: 1})
	_ = fourWorkers.Store().Close()
	requireSameRuns(t, r1, r4)
	requireSameRuns(t, baseline, r4)

	resumeDB := filepath.Join(f.dir, "resume.db")
	first := openWS(t, f, resumeDB, "resume")
	segment := runWS(t, first, engine.RunOptions{Mode: "resume", MaxWorkers: 2, MaxRetries: 1, SuspendAfter: 2})
	if fmt.Sprint(segment.CommitSequence) != "[0 1]" || !segment.Suspended {
		t.Fatalf("first resume segment commits=%v suspended=%v", segment.CommitSequence, segment.Suspended)
	}
	_ = first.Store().Close()
	restarted := openWS(t, f, resumeDB, "resume")
	resumed := runWS(t, restarted, engine.RunOptions{Mode: "resume", MaxWorkers: 2, MaxRetries: 1})
	_ = restarted.Store().Close()
	if fmt.Sprint(resumed.ReusedWindows) != "[0 1]" {
		t.Fatalf("reused=%v", resumed.ReusedWindows)
	}
	requireSameRuns(t, baseline, resumed)
}

func TestIncrementalEqualsFullAndReusesUnaffectedWindows(t *testing.T) {
	f := newFixture(t)
	inc := openWS(t, f, filepath.Join(f.dir, "inc.db"), "inc")
	_ = runWS(t, inc, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	opacity := 0.55
	inv, err := inc.EditLayer("prop", nil, &opacity)
	if err != nil {
		t.Fatal(err)
	}
	if got := invalidationWindows(inv); fmt.Sprint(got) != "[1 2 3 4]" {
		t.Fatalf("invalidated windows=%v", got)
	}
	if fmt.Sprint(inv.LayerIDs) != "[prop]" {
		t.Fatalf("invalidated layers=%v", inv.LayerIDs)
	}
	incremental := runWS(t, inc, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	_ = inc.Store().Close()
	if fmt.Sprint(incremental.ReusedWindows) != "[0 5]" || fmt.Sprint(incremental.RecomputedWindows) != "[1 2 3 4]" {
		t.Fatalf("reused=%v recomputed=%v", incremental.ReusedWindows, incremental.RecomputedWindows)
	}

	fullWS := openWS(t, f, filepath.Join(f.dir, "full-after-edit.db"), "full-after-edit")
	if _, err := fullWS.EditLayer("prop", nil, &opacity); err != nil {
		t.Fatal(err)
	}
	full := runWS(t, fullWS, engine.RunOptions{Mode: "full", MaxWorkers: 3, ForceFull: true, MaxRetries: 1})
	_ = fullWS.Store().Close()
	requireSameRuns(t, full, incremental)
}

func TestHiddenLayerEditInvalidatesOnlyExportPhases(t *testing.T) {
	f := newFixture(t)
	db := filepath.Join(f.dir, "hidden.db")
	ws := openWS(t, f, db, "hidden")
	_ = runWS(t, ws, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	hidden := false
	first, err := ws.EditLayer("character", &hidden, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(invalidationWindows(first)) != "[1 2 3]" {
		t.Fatalf("visibility edit windows=%v", invalidationWindows(first))
	}
	_ = runWS(t, ws, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	opacity := 0.23
	second, err := ws.EditLayer("character", nil, &opacity)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Windows) != 0 {
		t.Fatalf("hidden layer edit invalidated windows: %+v", second.Windows)
	}
	if len(second.Exports) != 3 || fmt.Sprint(second.LayerIDs) != "[character]" {
		t.Fatalf("export/layer invalidation=%+v", second)
	}
	status, err := ws.ExportStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status["export:v1"] {
		t.Fatalf("export:v1 should be invalid: %v", status)
	}
	after := runWS(t, ws, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	if len(after.RecomputedWindows) != 0 || len(after.ReusedWindows) != 6 {
		t.Fatalf("hidden edit should reuse all frames: reused=%v recomputed=%v", after.ReusedWindows, after.RecomputedWindows)
	}
	_ = ws.Store().Close()
}

func TestCancelAndResume(t *testing.T) {
	f := newFixture(t)
	baselineWS := openWS(t, f, filepath.Join(f.dir, "baseline.db"), "baseline")
	baseline := runWS(t, baselineWS, engine.RunOptions{Mode: "full", MaxWorkers: 3, ForceFull: true, MaxRetries: 1})
	_ = baselineWS.Store().Close()

	db := filepath.Join(f.dir, "cancel.db")
	canceledWS := openWS(t, f, db, "cancel")
	canceled, err := canceledWS.Run(context.Background(), engine.RunOptions{
		Mode: "realtime", MaxWorkers: 3, MaxRetries: 1, CancelAfter: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !canceled.Canceled || fmt.Sprint(canceled.CommitSequence) != "[0 1]" {
		t.Fatalf("canceled result commits=%v canceled=%v", canceled.CommitSequence, canceled.Canceled)
	}
	_ = canceledWS.Store().Close()
	resumedWS := openWS(t, f, db, "cancel")
	resumed := runWS(t, resumedWS, engine.RunOptions{Mode: "resume", MaxWorkers: 3, MaxRetries: 1})
	_ = resumedWS.Store().Close()
	requireSameRuns(t, baseline, resumed)
}

func TestFaultIsolationRetryAndResourceQueue(t *testing.T) {
	f := newFixture(t)
	baselineWS := openWS(t, f, filepath.Join(f.dir, "baseline.db"), "baseline")
	baseline := runWS(t, baselineWS, engine.RunOptions{Mode: "full", MaxWorkers: 3, ForceFull: true, MaxRetries: 1})
	_ = baselineWS.Store().Close()

	faultWS := openWS(t, f, filepath.Join(f.dir, "fault.db"), "fault")
	faulted := runWS(t, faultWS, engine.RunOptions{
		Mode: "realtime", MaxWorkers: 3, MaxRetries: 0,
		Faults: map[int]error{1: model.NewError(model.CodeDecode, "layers.prop.pixels", "injected")},
	})
	if _, ok := faulted.FailedWindows[1]; !ok || fmt.Sprint(faulted.CommitSequence) != "[0]" {
		t.Fatalf("failed=%v commits=%v", faulted.FailedWindows, faulted.CommitSequence)
	}
	recovered := runWS(t, faultWS, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	_ = faultWS.Store().Close()
	requireSameRuns(t, baseline, recovered)

	need := f.scene.FrameBytes() * 9
	bufferWS := openWS(t, f, filepath.Join(f.dir, "buffer.db"), "buffer")
	buffered := runWS(t, bufferWS, engine.RunOptions{
		Mode: "realtime", MaxWorkers: 2, MaxRetries: 1,
		PixelBufferBytesPerWorker: need, PixelBufferTotalBytes: need + need/2,
	})
	_ = bufferWS.Store().Close()
	if len(buffered.BufferEvents) == 0 {
		t.Fatal("expected queued buffer events")
	}
	requireSameRuns(t, baseline, buffered)
}

func TestPoisonedCheckpointInvalidatesWholeSnapshot(t *testing.T) {
	f := newFixture(t)
	db := filepath.Join(f.dir, "poison.db")
	ws := openWS(t, f, db, "poison")
	baseline := runWS(t, ws, engine.RunOptions{Mode: "full", MaxWorkers: 3, ForceFull: true, MaxRetries: 1})
	_ = ws.Store().Close()
	sqlDB, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE checkpoints SET content_hash='bad' WHERE window_index=1`); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openWS(t, f, db, "poison")
	result := runWS(t, reopened, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	_ = reopened.Store().Close()
	if !result.CacheInvalidated {
		t.Fatal("poisoned snapshot was not invalidated")
	}
	if len(result.ReusedWindows) != 0 {
		t.Fatalf("reused poisoned cache: %v", result.ReusedWindows)
	}
	requireSameRuns(t, baseline, result)
}

func TestSampleConfigurationIsPartOfCacheValidity(t *testing.T) {
	f := newFixture(t)
	db := filepath.Join(f.dir, "samples.db")
	ws := openWS(t, f, db, "samples")
	baseline := runWS(t, ws, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	custom := []model.SamplePoint{{X: 0, Y: 0}, {X: f.scene.Width - 1, Y: f.scene.Height - 1}}
	changed := runWS(t, ws, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1, Samples: custom})
	if !changed.CacheInvalidated || len(changed.ReusedWindows) != 0 {
		t.Fatalf("sample change did not invalidate cache: %+v", changed)
	}
	for i := range baseline.Frames {
		if baseline.Frames[i].Hash != changed.Frames[i].Hash {
			t.Fatalf("frame %d pixels changed with sample configuration", i)
		}
		if len(changed.Frames[i].Stats.Samples) != len(custom) {
			t.Fatalf("frame %d stats did not use custom samples", i)
		}
	}
	restored := runWS(t, ws, engine.RunOptions{Mode: "realtime", MaxWorkers: 3, MaxRetries: 1})
	if !restored.CacheInvalidated {
		t.Fatal("returning to default samples should invalidate cached custom stats")
	}
	requireSameRuns(t, baseline, restored)
	_ = ws.Store().Close()
}

func TestStateMachineIllegalTransitions(t *testing.T) {
	win := engine.NewWindow(0, 0, 3)
	if err := win.Commit(&model.WindowSnapshot{}); !model.IsCode(err, model.CodeState) || win.State() != engine.StatePending {
		t.Fatalf("pending commit err=%v state=%s", err, win.State())
	}
	if err := win.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := win.Begin(); !model.IsCode(err, model.CodeState) {
		t.Fatalf("double begin err=%v", err)
	}
	snap := &model.WindowSnapshot{WindowIndex: 0, StartFrame: 0, EndFrame: 3, Width: 1, Height: 1, Channels: 4,
		FrameDurationNS: 1, SourceVersion: "v1"}
	if err := win.Commit(snap); err != nil {
		t.Fatal(err)
	}
	if err := win.Fail("late failure"); !model.IsCode(err, model.CodeState) || win.State() != engine.StateCommitted {
		t.Fatalf("committed fail err=%v state=%s", err, win.State())
	}
	if err := win.Invalidate([]string{"layer"}, "edit"); err != nil || win.State() != engine.StatePending {
		t.Fatalf("invalidate err=%v state=%s", err, win.State())
	}
}

func TestRunRejectsOutOfRangeSample(t *testing.T) {
	f := newFixture(t)
	ws := openWS(t, f, filepath.Join(f.dir, "bad-sample.db"), "bad-sample")
	defer ws.Store().Close()
	_, err := ws.Run(context.Background(), engine.RunOptions{
		Mode: "realtime", MaxWorkers: 2, Samples: []model.SamplePoint{{X: f.scene.Width, Y: 0}},
	})
	if !model.IsCode(err, model.CodeParam) {
		t.Fatalf("got %v, want E_PARA", err)
	}
	e, _ := model.AsError(err)
	if e.Path != "sample_points[0]" {
		t.Fatalf("path=%s", e.Path)
	}
}

func invalidationWindows(inv *engine.Invalidation) []int {
	out := make([]int, 0, len(inv.Windows))
	for _, item := range inv.Windows {
		out = append(out, item.Window)
	}
	return out
}
