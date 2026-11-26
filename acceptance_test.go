package screenrelay_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"screenrelay/internal/checkpoint"
	"screenrelay/internal/engine"
	"screenrelay/internal/mapping"
	"screenrelay/internal/model"
	"screenrelay/internal/sample"
	"screenrelay/internal/schema"
	"screenrelay/internal/statemachine"
	"screenrelay/internal/verify"
	"screenrelay/internal/xconv"
)

func TestThreeVersionsLoadAndIntegerRoundTrip(t *testing.T) {
	set, err := sample.Generate(sample.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	loaded := map[int]*model.Project{}
	for _, major := range []int{1, 2, 3} {
		project, report, err := schema.Load(set.Raw[versionName(major)])
		if err != nil {
			t.Fatalf("load v%d: %v", major, err)
		}
		if project.SchemaMajor != major {
			t.Fatalf("major=%d got %d", major, project.SchemaMajor)
		}
		if len(project.Windows) != set.Truth.WindowCount {
			t.Fatalf("v%d windows=%d", major, len(project.Windows))
		}
		loaded[major] = project
		if major == 1 && len(report.Fills) != 0 {
			t.Fatalf("generated v1 unexpectedly has fills: %d", len(report.Fills))
		}
	}

	fixture, err := sample.V1MissingDefaultsFixture(set)
	if err != nil {
		t.Fatal(err)
	}
	_, fills, err := schema.Load(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(fills.Fills) < 2 {
		t.Fatalf("expected scale/chroma default fills, got %+v", fills.Fills)
	}

	for _, lowMajor := range []int{1, 2} {
		lowBytes, _, err := schema.Export(loaded[3], lowMajor)
		if err != nil {
			t.Fatal(err)
		}
		low, _, err := schema.Load(lowBytes)
		if err != nil {
			t.Fatal(err)
		}
		highBytes, _, err := schema.Export(low, 3)
		if err != nil {
			t.Fatal(err)
		}
		highAgain, _, err := schema.Load(highBytes)
		if err != nil {
			t.Fatal(err)
		}
		for i := range loaded[3].Windows {
			wantRegion := loaded[3].Windows[i].Region
			gotRegion := highAgain.Windows[i].Region
			if gotRegion != wantRegion {
				t.Fatalf("v%d window %d region roundtrip: %+v != %+v", lowMajor, i, gotRegion, wantRegion)
			}
			if highAgain.Windows[i].Tick != loaded[3].Windows[i].Tick {
				t.Fatalf("v%d window %d tick roundtrip", lowMajor, i)
			}
		}
		for i := range loaded[3].Events {
			if highAgain.Events[i].Tick != loaded[3].Events[i].Tick {
				t.Fatalf("v%d event %d tick roundtrip", lowMajor, i)
			}
		}
	}
}

func TestMappingBoundsOrderAndErrors(t *testing.T) {
	set, err := sample.Generate(sample.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	window := set.Project.Windows[0]
	var scaleResiduals, clip int64
	first := map[string]*mapping.Result{}
	for _, screen := range set.Project.Screens {
		result, err := mapping.MapScreen(mapping.Request{Region: window.Region, Screen: screen, TimeQuantum: 1}, window.Tick)
		if err != nil {
			t.Fatalf("screen %s: %v", screen.ID, err)
		}
		first[screen.ID] = result
		assertBounds(t, result)
		scaleResiduals += result.Errors.ScaleRoundingNumerator
		clip += result.Errors.ClipTruncationPixels
	}
	if scaleResiduals == 0 {
		t.Fatal("expected at least one nonzero integer scale residual")
	}
	if clip == 0 {
		t.Fatal("expected clipping truncation in sample topology")
	}

	reversed := append([]model.Screen(nil), set.Project.Screens...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	for _, screen := range reversed {
		result, err := mapping.MapScreen(mapping.Request{Region: window.Region, Screen: screen, TimeQuantum: 1}, window.Tick)
		if err != nil {
			t.Fatal(err)
		}
		if result.Params != first[screen.ID].Params || result.PixelRegion != first[screen.ID].PixelRegion {
			t.Fatalf("order changed result for %s", screen.ID)
		}
	}

	bad := model.Screen{
		ID: "bad", PixelW: 10, PixelH: 10, Scale: model.Scale100,
		SupportedScales: []model.ScaleTier{model.Scale100},
		Chroma:          model.Chroma420, SupportedChromas: []model.Chroma{model.Chroma420},
	}
	_, err = mapping.MapScreen(mapping.Request{
		Region: model.Rect{W: model.InternalCoordUnit, H: model.InternalCoordUnit},
		Screen: bad, Requested: model.Scale200,
	}, 0)
	if !model.HasCode(err, model.EPara) {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	coded, _ := model.AsError(err)
	if coded.ScreenID != "bad" || coded.Actual == "" {
		t.Fatalf("E_PARA must identify screen and actual capabilities: %+v", coded)
	}

	if _, err := xconv.MulChecked(1<<62, 8, "overflow.field"); !model.HasCode(err, model.EOvf) {
		t.Fatalf("expected E_OVF, got %v", err)
	}
}

func TestThreePathsAndCheckpointRecovery(t *testing.T) {
	ctx := context.Background()
	set, err := sample.Generate(sample.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	budget := engine.DefaultBudget()
	full, err := engine.Run(ctx, set.Project, engine.Options{Budget: budget})
	if err != nil {
		t.Fatal(err)
	}

	realtimeDB := filepath.Join(t.TempDir(), "realtime.checkpoint")
	realtimeStore, err := checkpoint.Open(realtimeDB)
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := engine.Run(ctx, set.Project, engine.Options{Budget: budget, Store: realtimeStore})
	_ = realtimeStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := engine.CompareResults(full, realtime); mismatch != "" {
		t.Fatal(mismatch)
	}

	recoveryDB := filepath.Join(t.TempDir(), "recovery.checkpoint")
	partialStore, err := checkpoint.Open(recoveryDB)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := engine.Run(ctx, set.Project, engine.Options{Budget: budget, Store: partialStore, StopAfter: 2})
	_ = partialStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(partial.CommitSequence) != 2 {
		t.Fatalf("partial commits=%v", partial.CommitSequence)
	}
	resumeStore, err := checkpoint.Open(recoveryDB)
	if err != nil {
		t.Fatal(err)
	}
	recovered, recovery, err := engine.Resume(ctx, set.Project, resumeStore, engine.Options{Budget: budget, Store: resumeStore})
	_ = resumeStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	if recovery.InvalidSeq != 0 {
		t.Fatalf("unexpected invalid checkpoint: %s", recovery.InvalidReason)
	}
	if mismatch := engine.CompareResults(full, recovered); mismatch != "" {
		t.Fatal(mismatch)
	}

	var sawReplay, sawExit bool
	for _, window := range recovered.Windows {
		for _, screen := range window.ScreenResults {
			if window.Seq == 3 && screen.ScreenID == "scr-e" {
				if screen.Replay == nil || screen.Replay.SourceWindowSeq != 2 {
					t.Fatalf("join replay missing: %+v", screen.Replay)
				}
				sawReplay = true
			}
			if window.Seq == 6 && screen.ScreenID == "scr-c" && screen.Dispatch != nil {
				t.Fatal("exited screen received packets")
			}
		}
		if window.Seq == 6 {
			for _, id := range window.ActiveScreenIDs {
				if id == "scr-c" {
					t.Fatal("scr-c remained active")
				}
			}
			sawExit = true
		}
	}
	if !sawReplay || !sawExit {
		t.Fatalf("replay=%v exit=%v", sawReplay, sawExit)
	}
}

func TestStateMachinesRejectIllegalTransition(t *testing.T) {
	session := statemachine.NewSession()
	if err := session.Transition(statemachine.SessionCasting); !model.HasCode(err, model.EState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if session.Current != statemachine.SessionEstablishing {
		t.Fatalf("state changed after illegal transition: %s", session.Current)
	}
	window := statemachine.NewWindow(1)
	if err := window.Transition(statemachine.WindowCommitted); !model.HasCode(err, model.EState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
}

func TestVerificationSuiteReproducesExceptionChains(t *testing.T) {
	report, err := verify.RunAll(context.Background(), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		for _, check := range report.Checks {
			if !check.OK {
				t.Errorf("%s: %s", check.Name, check.Detail)
			}
		}
		t.Fatal("verification suite failed")
	}
}

func TestLowVersionExportRestoresCompatibleTopologyAndCapabilities(t *testing.T) {
	set, err := sample.Generate(sample.FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	project := *set.Project
	project.Screens = append([]model.Screen(nil), set.Project.Screens...)
	for i := range project.Screens {
		makeV1Compatible(&project.Screens[i])
	}
	project.Events = append([]model.TopologyEvent(nil), set.Project.Events...)
	for i := range project.Events {
		if project.Events[i].Screen != nil {
			screen := *project.Events[i].Screen
			makeV1Compatible(&screen)
			project.Events[i].Screen = &screen
		}
	}
	data, lossy, err := schema.Export(&project, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(lossy.Lossy) != 0 {
		t.Fatalf("compatible export should be lossless, got %d", len(lossy.Lossy))
	}
	loaded, _, err := schema.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	want := mapping.SortedScreens(project.Screens)
	got := mapping.SortedScreens(loaded.Screens)
	if len(want) != len(got) {
		t.Fatalf("screen count %d != %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Origin != want[i].Origin || got[i].PixelW != want[i].PixelW || got[i].PixelH != want[i].PixelH {
			t.Fatalf("topology mismatch %+v != %+v", got[i], want[i])
		}
		if got[i].Scale != want[i].Scale || got[i].Chroma != want[i].Chroma {
			t.Fatalf("capability current mismatch %+v != %+v", got[i], want[i])
		}
		if strings.Join(enumStrings(got[i].SupportedScales), ",") != strings.Join(enumStrings(want[i].SupportedScales), ",") {
			t.Fatalf("scale caps mismatch %v != %v", got[i].SupportedScales, want[i].SupportedScales)
		}
	}
}

func TestCompareLocatesFirstMismatch(t *testing.T) {
	base := &engine.RunResult{Windows: []engine.WindowResult{{Seq: 1, State: "committed"}, {Seq: 2, State: "committed"}}}
	changed := &engine.RunResult{Windows: []engine.WindowResult{{Seq: 1, State: "committed"}, {Seq: 2, State: "failed"}}}
	message := engine.CompareResults(base, changed)
	if !strings.Contains(message, "windows[1]") || !strings.Contains(message, "committed != failed") {
		t.Fatalf("unexpected mismatch: %s", message)
	}
	index, _, detail := engine.FirstMismatch(base, changed)
	if index != 1 || detail == "" {
		t.Fatalf("index=%d detail=%q", index, detail)
	}
}

func TestPoolIsolatesTaskErrors(t *testing.T) {
	good := model.Screen{
		ID: "good", PixelW: 1280, PixelH: 720, Scale: model.Scale100,
		Origin: model.Point{}, SupportedScales: []model.ScaleTier{model.Scale100},
		Chroma: model.Chroma420, SupportedChromas: []model.Chroma{model.Chroma420},
	}
	bad := good
	bad.ID = "bad"
	outcomes := engine.RunScreenPool(context.Background(), engine.DefaultBudget(), []engine.ScreenTask{
		{WindowSeq: 1, Tick: 90, Region: model.Rect{W: 100 * model.InternalCoordUnit, H: 100 * model.InternalCoordUnit}, Screen: good, MaxPacketBytes: 4096},
		{WindowSeq: 1, Tick: 90, Region: model.Rect{W: 100 * model.InternalCoordUnit, H: 100 * model.InternalCoordUnit}, Screen: bad, RequestedScale: model.Scale200, MaxPacketBytes: 4096},
	})
	var goodSeen, badSeen bool
	for _, outcome := range outcomes {
		if outcome.ScreenID == "good" {
			goodSeen = true
			if outcome.Err != nil || outcome.Dispatch == nil {
				t.Fatalf("good task failed: %v", outcome.Err)
			}
		}
		if outcome.ScreenID == "bad" {
			badSeen = true
			if !model.HasCode(outcome.Err, model.EPara) {
				t.Fatalf("bad task error=%v", outcome.Err)
			}
		}
	}
	if !goodSeen || !badSeen {
		t.Fatalf("good=%v bad=%v", goodSeen, badSeen)
	}
}

func TestNonAlignedResidualsStayInsideBounds(t *testing.T) {
	screen := model.Screen{
		ID: "nonaligned", PixelW: 10000, PixelH: 10000,
		Origin: model.Point{X: 13, Y: 29}, Scale: model.Scale125,
		SupportedScales:  []model.ScaleTier{model.Scale125},
		Chroma:           model.Chroma420,
		SupportedChromas: []model.Chroma{model.Chroma420},
	}
	result, err := mapping.MapScreen(mapping.Request{
		Region: model.Rect{X: 107, Y: 311, W: 333*model.InternalCoordUnit + 17, H: 219*model.InternalCoordUnit + 31},
		Screen: screen, TimeQuantum: 1500, PacketBytes: 65536,
	}, 3001)
	if err != nil {
		t.Fatal(err)
	}
	if result.Errors.CoordRemainderUnits == 0 || result.Errors.ScaleRoundingNumerator == 0 || result.Errors.TimeRemainderTicks != 1 {
		t.Fatalf("expected nonzero coordinate/scale/time residuals, got %+v", result.Errors)
	}
	assertBounds(t, result)
}

func enumStrings[T ~string](items []T) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = string(item)
	}
	return out
}

func makeV1Compatible(screen *model.Screen) {
	screen.SupportedScales = []model.ScaleTier{model.Scale100, model.Scale200}
	if screen.Scale != model.Scale100 && screen.Scale != model.Scale200 {
		screen.Scale = model.Scale100
	}
	screen.SupportedChromas = []model.Chroma{model.Chroma420, model.Chroma444}
	if screen.Chroma == model.Chroma422 {
		screen.Chroma = model.Chroma420
	}
}

func assertBounds(t *testing.T, result *mapping.Result) {
	t.Helper()
	e := result.Errors
	b := e.Bounds
	if e.CoordRemainderUnits > b.CoordRemainderUnits {
		t.Fatalf("coord bound: %+v", e)
	}
	if e.ScaleRoundingNumerator > b.ScaleRoundingNumerator {
		t.Fatalf("scale bound: %+v", e)
	}
	if e.ClipTruncationPixels > b.ClipTruncationPixels {
		t.Fatalf("clip bound: %+v", e)
	}
	if e.TimeRemainderTicks > b.TimeRemainderTicks {
		t.Fatalf("time bound: %+v", e)
	}
}

func versionName(major int) string {
	return fmt.Sprintf("v%d", major)
}
