package lightshow_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"lightshow"
	"lightshow/sample"
	_ "modernc.org/sqlite"
)

func generate(t *testing.T) *sample.Generated {
	t.Helper()
	generated, err := sample.Generate(t.TempDir(), 421337)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return generated
}

func TestGeneratedThreeVersionsMatchTruth(t *testing.T) {
	generated := generate(t)
	cases := []struct {
		name    string
		file    string
		version lightshow.Version
	}{
		{"v1", "show.v1.json", lightshow.Version1},
		{"v2", "show.v2.json", lightshow.Version2},
		{"v3", "show.v3.json", lightshow.Version3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			show, err := lightshow.Load(generated.Files[tc.file])
			if err != nil {
				t.Fatalf("load %s: %v", tc.name, err)
			}
			if show.SourceVersion != tc.version {
				t.Fatalf("source version = %s, want %s", show.SourceVersion, tc.version)
			}
			schedule, err := lightshow.Schedule(show)
			if err != nil {
				t.Fatalf("schedule %s: %v", tc.name, err)
			}
			if len(schedule.Frames) != 180 {
				t.Fatalf("frames = %d, want 180", len(schedule.Frames))
			}
			if err := sample.VerifySchedule(tc.version, schedule, generated.Truth); err != nil {
				t.Fatal(err)
			}
			for _, window := range schedule.Windows {
				if window.ContentHash != window.ComputeContentHash() {
					t.Fatalf("window %s hash is not canonical", window.WindowID)
				}
			}
		})
	}
}

func TestColorAndTimeRoundTrips(t *testing.T) {
	step, err := lightshow.ParseColorStep("1/255")
	if err != nil {
		t.Fatal(err)
	}
	for q := 0; q <= 255; q++ {
		want := uint16(q) * 257
		norm := float64(q) / 255
		got, err := lightshow.NormalizedToRGB16([3]any{norm, norm, norm}, step)
		if err != nil {
			t.Fatal(err)
		}
		if got.R != want || got.G != want || got.B != want {
			t.Fatalf("normalized %d/255 -> %v, want %d", q, got, want)
		}
		r, g, b := lightshow.RGB16To8(got)
		if int(r) != q || int(g) != q || int(b) != q {
			t.Fatalf("8-bit roundtrip %d -> %d/%d/%d", q, r, g, b)
		}
	}
	for _, mode := range []lightshow.TimeRounding{lightshow.RoundNearest, lightshow.RoundFloor, lightshow.RoundCeil} {
		for frame := int64(0); frame <= 180; frame++ {
			us, err := lightshow.FrameToUS(frame, 30, mode)
			if err != nil {
				t.Fatal(err)
			}
			back, err := lightshow.USToFrame(us, 30, mode)
			if err != nil {
				t.Fatal(err)
			}
			if diff := back - frame; diff < -1 || diff > 1 {
				t.Fatalf("%s frame roundtrip %d -> %d -> %d", mode, frame, us, back)
			}
		}
	}
	for _, ms := range []int64{0, 1, 1300, 5100, 6000} {
		if got := lightshow.USToMS(lightshow.MSToUS(ms), lightshow.RoundNearest); got != ms {
			t.Fatalf("ms roundtrip %d -> %d", ms, got)
		}
	}
	if lightshow.ColorTolerance(lightshow.Version1) != 257 {
		t.Fatal("v1 color tolerance must be one 8-bit step")
	}
	if lightshow.TimeTolerance(30) != 33334 {
		t.Fatalf("time tolerance = %d", lightshow.TimeTolerance(30))
	}
}

func TestHighLowHighRoundTrip(t *testing.T) {
	generated := generate(t)
	original, err := lightshow.Load(generated.Files["show.v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	originalSchedule, err := lightshow.Schedule(original)
	if err != nil {
		t.Fatal(err)
	}
	v2Document, err := original.Export(lightshow.Version2)
	if err != nil {
		t.Fatal(err)
	}
	v2Show, err := lightshow.Load(v2Document.Data)
	if err != nil {
		t.Fatal(err)
	}
	v3Document, err := v2Show.Export(lightshow.Version3)
	if err != nil {
		t.Fatal(err)
	}
	v3Again, err := lightshow.Load(v3Document.Data)
	if err != nil {
		t.Fatal(err)
	}
	roundTripSchedule, err := lightshow.Schedule(v3Again)
	if err != nil {
		t.Fatal(err)
	}
	originalRun := scheduleRun(originalSchedule)
	roundTripRun := scheduleRun(roundTripSchedule)
	if mismatch := lightshow.CompareRuns(originalRun, roundTripRun); mismatch != nil {
		t.Fatal(mismatch)
	}

	v1Document, err := original.Export(lightshow.Version1)
	if err != nil {
		t.Fatal(err)
	}
	v1Show, err := lightshow.Load(v1Document.Data)
	if err != nil {
		t.Fatal(err)
	}
	v1Schedule, err := lightshow.Schedule(v1Show)
	if err != nil {
		t.Fatal(err)
	}
	for frameIndex := range originalSchedule.Frames {
		want := originalSchedule.Frames[frameIndex]
		got := v1Schedule.Frames[frameIndex]
		if want.AtUS != got.AtUS || len(want.Lights) != len(got.Lights) {
			t.Fatalf("v1 roundtrip frame %d shape mismatch", frameIndex)
		}
		for lightIndex := range want.Lights {
			a, b := want.Lights[lightIndex], got.Lights[lightIndex]
			if lightshow.ColorTolerance(lightshow.Version1) < testAbs(int(a.Color.R)-int(b.Color.R)) ||
				lightshow.ColorTolerance(lightshow.Version1) < testAbs(int(a.Color.G)-int(b.Color.G)) ||
				lightshow.ColorTolerance(lightshow.Version1) < testAbs(int(a.Color.B)-int(b.Color.B)) ||
				a.Intensity != b.Intensity || a.Position != b.Position || a.Focus != b.Focus {
				t.Fatalf("v1 roundtrip frame %d fixture %s outside tolerance", frameIndex, a.FixtureID)
			}
		}
	}
}

func testAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func scheduleRun(schedule *lightshow.ScheduleResult) *lightshow.RunResult {
	return &lightshow.RunResult{
		Frames: schedule.Frames, Decisions: schedule.Decisions,
		Consumption: schedule.Consumption, Windows: schedule.Windows,
	}
}

func TestVersionLoadErrors(t *testing.T) {
	generated := generate(t)
	v3 := generated.Files["show.v3.json"]
	var root map[string]any
	if err := json.Unmarshal(v3, &root); err != nil {
		t.Fatal(err)
	}

	unknown := cloneMap(t, root)
	unknown["formatVersion"] = 99
	if _, err := lightshow.Load(marshal(t, unknown)); !lightshow.IsCode(err, lightshow.CodeVersion) {
		t.Fatalf("unknown version error = %v", err)
	}

	missing := cloneMap(t, root)
	delete(missing["production"].(map[string]any), "fps")
	if _, err := lightshow.Load(marshal(t, missing)); !lightshow.IsCode(err, lightshow.CodeVersion) {
		t.Fatalf("missing field error = %v", err)
	} else if e, _ := lightshow.AsEngineError(err); e.Path != "production.fps" {
		t.Fatalf("missing field path = %q", e.Path)
	}

	order := cloneMap(t, root)
	changes := order["changes"].([]any)
	changes[1].(map[string]any)["at_ms"] = 6000
	if _, err := lightshow.Load(marshal(t, order)); !lightshow.IsCode(err, lightshow.CodeOrder) {
		t.Fatalf("order error = %v", err)
	}

	cycle := cloneMap(t, root)
	groups := cycle["controlGroups"].([]any)
	for _, raw := range groups {
		group := raw.(map[string]any)
		if group["id"] == "G_FRONT" {
			group["members"].(map[string]any)["groups"] = []any{"G_ALL"}
		}
	}
	if _, err := lightshow.Load(marshal(t, cycle)); !lightshow.IsCode(err, lightshow.CodeGroup) {
		t.Fatalf("cycle error = %v", err)
	}

	if _, err := lightshow.Load([]byte("{broken")); !lightshow.IsCode(err, lightshow.CodeDecode) {
		t.Fatalf("decode error = %v", err)
	}
}

func TestLossyExportAndExplicitUpgrade(t *testing.T) {
	generated := generate(t)
	show, err := lightshow.Load(generated.Files["show.v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	result, err := show.ExportStrict(lightshow.Version2)
	if !lightshow.IsCode(err, lightshow.CodeLossy) {
		t.Fatalf("strict v2 export error = %v", err)
	}
	if len(result.Data) == 0 || len(result.Losses) == 0 {
		t.Fatal("lossy export must still contain data and loss details")
	}
	e, _ := lightshow.AsEngineError(err)
	if len(e.Losses) != len(result.Losses) {
		t.Fatal("E_LOSSY must carry the full loss list")
	}
	foundNested := false
	for _, loss := range result.Losses {
		if strings.Contains(loss.Path, "members.groups") && loss.Disposition != "" {
			foundNested = true
		}
	}
	if !foundNested {
		t.Fatal("nested group loss was not reported")
	}

	v1, err := lightshow.Load(generated.Files["show.v1.json"])
	if err != nil {
		t.Fatal(err)
	}
	if len(v1.UpgradeTraces) == 0 {
		t.Fatal("v1 upgrade must record defaulted fields")
	}
	for _, trace := range v1.UpgradeTraces {
		if trace.Path == "" || trace.DefaultSource == "" {
			t.Fatalf("incomplete upgrade trace: %+v", trace)
		}
	}
}

func TestThreeRunPathsCancelResumeAndCorruption(t *testing.T) {
	generated := generate(t)
	show, err := lightshow.Load(generated.Files["show.v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	realtimeStore, err := lightshow.OpenStore(filepath.Join(dir, "realtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer realtimeStore.Close()
	realtime, err := lightshow.Run(context.Background(), show, realtimeStore, lightshow.RunRealtime)
	if err != nil {
		t.Fatalf("realtime: %v", err)
	}

	fullStore, err := lightshow.OpenStore(filepath.Join(dir, "full.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fullStore.Close()
	full, err := lightshow.Run(context.Background(), show, fullStore, lightshow.RunFullRecompute)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if mismatch := lightshow.CompareRuns(realtime, full); mismatch != nil {
		t.Fatal(mismatch)
	}

	resumeStore, err := lightshow.OpenStore(filepath.Join(dir, "resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer resumeStore.Close()
	canceled, err := lightshow.RunWithOptions(context.Background(), show, resumeStore, lightshow.RunOptions{
		Mode: lightshow.RunRealtime, CancelAfter: 3,
	})
	if !lightshow.IsCode(err, lightshow.CodeCanceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if len(canceled.Windows) != 3 {
		t.Fatalf("canceled run persisted %d windows, want 3", len(canceled.Windows))
	}
	resumed, err := lightshow.Run(context.Background(), show, resumeStore, lightshow.RunResume)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.ReusedWindows != 3 {
		t.Fatalf("resumed reused %d windows, want 3", resumed.ReusedWindows)
	}
	if mismatch := lightshow.CompareRuns(realtime, resumed); mismatch != nil {
		t.Fatal(mismatch)
	}

	state, err := realtimeStore.State("window-0000")
	if err != nil {
		t.Fatal(err)
	}
	if state != lightshow.StateCommitted {
		t.Fatalf("state = %s", state)
	}
	if err := realtimeStore.Transition("window-0000", lightshow.StateScheduling); !lightshow.IsCode(err, lightshow.CodeState) {
		t.Fatalf("illegal transition error = %v", err)
	}
	if state, _ := realtimeStore.State("window-0000"); state != lightshow.StateCommitted {
		t.Fatalf("illegal transition changed state to %s", state)
	}

	dbPath := filepath.Join(dir, "realtime.db")
	if err := realtimeStore.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE outputs SET color_r = color_r + 1 WHERE window_id = 'window-0000' AND frame = 0 AND fixture_id = 'L1'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	corruptStore, err := lightshow.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer corruptStore.Close()
	err = corruptStore.ValidateCommitted()
	if !lightshow.IsCode(err, lightshow.CodeDecode) {
		t.Fatalf("corruption error = %v", err)
	}
	if !strings.HasPrefix(err.Error(), "E_DECODE at checkpoint.content_hash.window-0000") {
		t.Fatalf("unexpected corruption path: %v", err)
	}
}

func TestIncrementalInvalidationAndFullEquality(t *testing.T) {
	generated := generate(t)
	show, err := lightshow.Load(generated.Files["show.v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := lightshow.OpenStore(filepath.Join(dir, "incremental.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	initial, err := lightshow.Run(context.Background(), show, store, lightshow.RunRealtime)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := lightshow.NewIncrementalEngine(store, show)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := engine.EditChange("C30_L4", func(change *lightshow.Change) error {
		position := [3]int{400, -400, 600}
		change.Values.Position = &position
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(edited.Report.Invalidated, []int{10, 11}) {
		t.Fatalf("invalidated = %v, want [10 11]", edited.Report.Invalidated)
	}
	if len(edited.Report.Reused) != 10 {
		t.Fatalf("reused = %d, want 10", len(edited.Report.Reused))
	}
	wantExports := []string{"export:window-0010", "export:window-0011"}
	if !reflect.DeepEqual(edited.Report.Exports, wantExports) {
		t.Fatalf("exports = %v, want %v", edited.Report.Exports, wantExports)
	}

	fullStore, err := lightshow.OpenStore(filepath.Join(dir, "full-after-edit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fullStore.Close()
	full, err := lightshow.Run(context.Background(), engine.Show, fullStore, lightshow.RunFullRecompute)
	if err != nil {
		t.Fatal(err)
	}
	incremental, err := lightshow.Run(context.Background(), engine.Show, store, lightshow.RunResume)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := lightshow.CompareRuns(full, incremental); mismatch != nil {
		t.Fatal(mismatch)
	}
	if mismatch := lightshow.CompareRuns(initial, full); mismatch == nil {
		t.Fatal("an actual edit should change at least one output")
	}
}

func TestStandaloneStateMachine(t *testing.T) {
	fsm := lightshow.NewWindowFSM()
	if fsm.State() != lightshow.StatePending {
		t.Fatalf("initial state = %s", fsm.State())
	}
	if err := fsm.Transition(lightshow.StateScheduling); err != nil {
		t.Fatal(err)
	}
	if err := fsm.Transition(lightshow.StateCommitted); err != nil {
		t.Fatal(err)
	}
	err := fsm.Transition(lightshow.StateScheduling)
	if !lightshow.IsCode(err, lightshow.CodeState) {
		t.Fatalf("illegal transition error = %v", err)
	}
	if fsm.State() != lightshow.StateCommitted {
		t.Fatalf("illegal transition changed state to %s", fsm.State())
	}
	retry := lightshow.NewWindowFSM()
	if err := retry.Transition(lightshow.StateScheduling); err != nil {
		t.Fatal(err)
	}
	if err := retry.Transition(lightshow.StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := retry.Transition(lightshow.StateScheduling); err != nil {
		t.Fatalf("failed-to-scheduling retry: %v", err)
	}
}

func TestConcurrentSchedulingIsDeterministic(t *testing.T) {
	generated := generate(t)
	show, err := lightshow.Load(generated.Files["show.v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	results := make([]*lightshow.ScheduleResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = lightshow.Schedule(show)
		}(i)
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if mismatch := lightshow.CompareRuns(scheduleRun(results[0]), scheduleRun(results[i])); mismatch != nil {
			t.Fatalf("worker %d: %v", i, mismatch)
		}
	}
}

func TestFirstMismatchIdentifiesFrameAndFixture(t *testing.T) {
	generated := generate(t)
	show, err := lightshow.Load(generated.Files["show.v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := lightshow.Schedule(show)
	if err != nil {
		t.Fatal(err)
	}
	a := scheduleRun(schedule)
	b := scheduleRun(schedule)
	b.Frames = append([]lightshow.FrameOutput(nil), b.Frames...)
	for i := range b.Frames {
		b.Frames[i].Lights = append([]lightshow.LightOutput(nil), b.Frames[i].Lights...)
	}
	b.Frames[42].Lights[2].Intensity++
	mismatch := lightshow.CompareRuns(a, b)
	if mismatch == nil || mismatch.Frame != 42 || mismatch.FixtureID != "L3" {
		t.Fatalf("mismatch = %+v", mismatch)
	}
}

func cloneMap(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoadFileDecodeErrorWrapsReadFailure(t *testing.T) {
	_, err := lightshow.LoadFile(filepath.Join(t.TempDir(), "missing.json"))
	if !lightshow.IsCode(err, lightshow.CodeDecode) {
		t.Fatalf("read error = %v", err)
	}
	var e *lightshow.EngineError
	if !errors.As(err, &e) {
		t.Fatalf("error is not EngineError: %T", err)
	}
}

func TestGeneratedFilesAreDeterministic(t *testing.T) {
	dir := t.TempDir()
	first, err := sample.Generate(dir, 421337)
	if err != nil {
		t.Fatal(err)
	}
	secondDir := t.TempDir()
	second, err := sample.Generate(secondDir, 421337)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Files, second.Files) {
		t.Fatal("fixed seed generated different bytes")
	}
	for name := range first.Files {
		a, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(secondDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s is not deterministic", name)
		}
	}
}
