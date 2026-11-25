package kernel_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"obsproj/internal/kernel"
	"obsproj/internal/model"
	"obsproj/internal/runner"
	"obsproj/internal/scenario"
)

func TestMapRegionDeterministicAndCropped(t *testing.T) {
	screen := testScreen("A", 0, 0, 1280, 720, model.Scale150, model.Chroma420,
		[]model.Scale{100, 150, 200}, []model.Chroma{420, 422})
	source := model.Rect{X: -120000, Y: 60000, W: 1200000, H: 700000}
	first, metrics, err := kernel.MapRegion(screen, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, secondMetrics, secondErr := kernel.MapRegion(screen, source, 0)
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	if first != second || metrics != secondMetrics {
		t.Fatal("mapping is not deterministic")
	}
	want := model.PixelRect{X: 0, Y: 90, W: 1280, H: 630}
	if first.Pixels != want {
		t.Fatalf("pixels=%+v want=%+v", first.Pixels, want)
	}
	if first.CropLeft != 180 || first.CropRight != 340 || first.CropBottom != 420 {
		t.Fatalf("unexpected crop: %+v", first)
	}
	if metrics.ActualDeviation > metrics.TotalBound || !metrics.WithinBound {
		t.Fatalf("bound violation: %+v", metrics)
	}
}

func TestMapRegionParameterErrors(t *testing.T) {
	screen := testScreen("A", 0, 0, 1280, 720, model.Scale150, model.Chroma420,
		[]model.Scale{100, 150, 200}, []model.Chroma{420, 422})
	source := model.Rect{X: 0, Y: 0, W: 1000, H: 1000}

	badScale := screen
	badScale.Scale = model.Scale125
	_, _, scaleErr := kernel.MapRegion(badScale, source, 0)
	assertKernelError(t, scaleErr, model.CodeParam)
	if scaleErr.ScreenID != "A" || scaleErr.RequestedScale == nil || *scaleErr.RequestedScale != model.Scale125 ||
		len(scaleErr.AllowedScales) != 3 {
		t.Fatalf("incomplete E_PARA: %+v", scaleErr)
	}

	empty := source
	empty.X = 5000 * 1000
	_, _, cropErr := kernel.MapRegion(screen, empty, 0)
	assertKernelError(t, cropErr, model.CodeParam)
	if cropErr.ScreenID != "A" {
		t.Fatalf("crop error screen=%q", cropErr.ScreenID)
	}
}

func TestBoundedPoolQueuesInsteadOfDropping(t *testing.T) {
	jobs := []kernel.MapJob{
		{Window: 1, Screen: testScreen("A", 0, 0, 640, 360, 100, 420, []model.Scale{100}, []model.Chroma{420}), Source: model.Rect{X: 0, Y: 0, W: 500000, H: 300000}},
		{Window: 1, Screen: testScreen("B", 0, 0, 640, 360, 100, 420, []model.Scale{100}, []model.Chroma{420}), Source: model.Rect{X: 0, Y: 0, W: 500000, H: 300000}},
		{Window: 1, Screen: testScreen("C", 0, 0, 640, 360, 100, 420, []model.Scale{100}, []model.Chroma{420}), Source: model.Rect{X: 0, Y: 0, W: 500000, H: 300000}},
	}
	options := kernel.MapPoolOptions{Workers: 1, BufferPixels: 1024, DiagnosticJobDelay: 5 * time.Millisecond}
	results, observations, err := kernel.MapAll(jobs, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("results=%d", len(results))
	}
	foundQueue := false
	foundChunk := false
	for _, observation := range observations {
		if strings.Contains(observation.Reason, "all_worker_buffers_busy") && observation.WaitMicros > 0 {
			foundQueue = true
		}
		if strings.Contains(observation.Reason, "job_exceeds_per_thread_pixel_buffer_chunked") {
			foundChunk = true
		}
	}
	if !foundQueue || !foundChunk {
		t.Fatalf("queue telemetry missing: %+v", observations)
	}
}

func TestConcurrencyDoesNotChangeReplay(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	base := runner.DefaultOptions()
	base.Engine.MapPool.Workers = 1
	base.Engine.MapPool.BufferPixels = 2048
	serial, err := runner.RunFullReplay(bundle.Session, base)
	if err != nil {
		t.Fatal(err)
	}
	parallelOptions := base
	parallelOptions.Engine.MapPool.Workers = 4
	parallelOptions.Engine.MapPool.BufferPixels = 32768
	parallel, err := runner.RunFullReplay(bundle.Session, parallelOptions)
	if err != nil {
		t.Fatal(err)
	}
	if diff := runner.CompareResults(serial, parallel); diff != nil {
		t.Fatal(diff)
	}
}

func TestEventPriorityJoinReplayAndExitSuppression(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.RunFullReplay(bundle.Session, runner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	byWindow := map[int]model.WindowCommit{}
	for _, commit := range result.Commits {
		byWindow[commit.Window] = commit
		active := map[string]bool{}
		for _, screenID := range commit.ActiveScreens {
			active[screenID] = true
		}
		for _, screen := range commit.Screens {
			if !active[screen.ScreenID] {
				t.Fatalf("window %d delivered to exited screen %s", commit.Window, screen.ScreenID)
			}
		}
	}
	w2 := byWindow[2]
	assertDecision(t, w2, "B", model.EventExit, "ev-011", "ev-012")
	assertDecision(t, w2, "D", model.EventJoin, "ev-010", "")
	screenD := findScreenCommit(t, w2, "D")
	if len(screenD.ReplayWindows) != 1 || screenD.ReplayWindows[0] != 1 {
		t.Fatalf("D replay windows=%v", screenD.ReplayWindows)
	}
	if !hasReplayPacket(screenD, 1) {
		t.Fatal("new join D did not receive committed window-1 replay packet")
	}

	w4 := byWindow[4]
	assertDecision(t, w4, "C", model.EventExit, "ev-030", "ev-031")
	for _, screen := range w4.Screens {
		if screen.ScreenID == "C" {
			t.Fatal("exited screen C received window-4 packets")
		}
	}
	w5 := byWindow[5]
	assertDecision(t, w5, "B", model.EventJoin, "ev-040", "ev-041")
	screenB := findScreenCommit(t, w5, "B")
	if len(screenB.ReplayWindows) != 1 || screenB.ReplayWindows[0] != 4 {
		t.Fatalf("B replay windows=%v", screenB.ReplayWindows)
	}
	w6 := byWindow[6]
	screenE := findScreenCommit(t, w6, "E")
	if len(screenE.ReplayWindows) != 1 || screenE.ReplayWindows[0] != 5 {
		t.Fatalf("E replay windows=%v", screenE.ReplayWindows)
	}
}

func TestInjectedKernelDiagnostics(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("E_ORDER", func(t *testing.T) {
		session := bundle.Session.Clone()
		for i := range session.Commands {
			if session.Commands[i].Window == 2 {
				session.Commands[i].At = -1
			}
		}
		_, newErr := kernel.NewEngine(session, kernel.DefaultEngineOptions())
		assertKernelError(t, newErr, model.CodeOrder)
		kerr, _ := model.AsKernelError(newErr)
		if kerr.Window != 2 || len(kerr.Operands) != 2 {
			t.Fatalf("order diagnostic=%+v", kerr)
		}
	})
	t.Run("E_CONFLICT", func(t *testing.T) {
		session := bundle.Session.Clone()
		duplicate := session.Events[0]
		session.Events = append(session.Events, duplicate)
		_, newErr := kernel.NewEngine(session, kernel.DefaultEngineOptions())
		assertKernelError(t, newErr, model.CodeConflict)
	})
	t.Run("E_CAP", func(t *testing.T) {
		session := bundle.Session.Clone()
		scale := model.Scale100
		session.Events = append(session.Events, model.Event{
			ID:       "ev-cap",
			Window:   3,
			At:       1490,
			Kind:     model.EventTopology,
			ScreenID: "C",
			Topology: &model.TopologyPatch{Scale: &scale},
		})
		engine, newErr := kernel.NewEngine(session, kernel.DefaultEngineOptions())
		if newErr != nil {
			t.Fatal(newErr)
		}
		var processErr error
		for _, window := range engine.WindowNumbers() {
			if _, _, processErr = engine.ProcessWindow(window); processErr != nil {
				break
			}
		}
		assertKernelError(t, processErr, model.CodeCapacity)
		kerr, _ := model.AsKernelError(processErr)
		if kerr.Window != 3 || kerr.ScreenID != "C" {
			t.Fatalf("capacity diagnostic=%+v", kerr)
		}
	})
	t.Run("E_SEG", func(t *testing.T) {
		options := kernel.DefaultEngineOptions()
		options.InjectWindowFailure = 5
		options.InjectScreenFailure = "B"
		engine, newErr := kernel.NewEngine(bundle.Session, options)
		if newErr != nil {
			t.Fatal(newErr)
		}
		for _, window := range engine.WindowNumbers() {
			_, _, processErr := engine.ProcessWindow(window)
			if window == 5 {
				assertKernelError(t, processErr, model.CodeSegment)
				kerr, _ := model.AsKernelError(processErr)
				if kerr.Window != 5 || kerr.ScreenID != "B" {
					t.Fatalf("segment diagnostic=%+v", kerr)
				}
				return
			}
			if processErr != nil {
				t.Fatal(processErr)
			}
		}
	})
	t.Run("E_BOUND", func(t *testing.T) {
		boundErr := kernel.ValidateMetricBound("A", 101, 100)
		assertKernelError(t, boundErr, model.CodeBound)
		if boundErr.ScreenID != "A" || boundErr.Actual == nil || *boundErr.Actual != 101 ||
			boundErr.Bound == nil || *boundErr.Bound != 100 {
			t.Fatalf("bound diagnostic=%+v", boundErr)
		}
	})
}

func TestStateMachinesRejectIllegalTransitionsWithoutProgressLoss(t *testing.T) {
	session := model.NewSessionFSM()
	session.AdvanceProgress(4)
	err := session.Transition(model.SessionStreaming)
	assertKernelError(t, err, model.CodeState)
	if session.State != model.SessionEstablished || session.Progress != 4 {
		t.Fatalf("session state changed: %+v", session)
	}
	if err := session.Transition(model.SessionNegotiating); err != nil {
		t.Fatal(err)
	}
	if err := session.Transition(model.SessionStreaming); err != nil {
		t.Fatal(err)
	}
	if err := session.Transition(model.SessionPaused); err != nil {
		t.Fatal(err)
	}
	if err := session.Transition(model.SessionStreaming); err != nil {
		t.Fatal(err)
	}
	if err := session.Transition(model.SessionEnded); err != nil {
		t.Fatal(err)
	}

	screen := model.NewScreenFSM()
	screen.AdvanceProgress(2)
	screenErr := screen.Transition(model.ScreenCommitted)
	assertKernelError(t, screenErr, model.CodeState)
	if screen.State != model.ScreenPending || screen.Progress != 2 {
		t.Fatalf("screen state changed: %+v", screen)
	}

	window := model.NewWindowFSM()
	window.AdvanceProgress(3)
	windowErr := window.Transition(model.WindowCommitted)
	assertKernelError(t, windowErr, model.CodeState)
	if window.State != model.WindowPending || window.Progress != 3 {
		t.Fatalf("window state changed: %+v", window)
	}
}

func TestEngineRejectsOutOfOrderWindowWithoutProgressChange(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	engine, newErr := kernel.NewEngine(bundle.Session, kernel.DefaultEngineOptions())
	if newErr != nil {
		t.Fatal(newErr)
	}
	_, _, processErr := engine.ProcessWindow(2)
	assertKernelError(t, processErr, model.CodeState)
	if engine.Window() != 0 {
		t.Fatalf("progress changed to %d", engine.Window())
	}
}

func testScreen(id string, ox, oy, w, h int64, scale model.Scale, chroma model.Chroma, scales []model.Scale, chromas []model.Chroma) model.Screen {
	return model.Screen{
		ID:         id,
		Name:       id,
		Origin:     model.Point{X: ox * 1000, Y: oy * 1000},
		Resolution: model.Size{W: w, H: h},
		Scale:      scale,
		Chroma:     chroma,
		Capability: model.Capability{
			Scales:        scales,
			Chromas:       chromas,
			MaxPacketSize: model.DefaultMaxPacketSize,
		},
	}
}

func assertDecision(t *testing.T, commit model.WindowCommit, screenID string, category model.EventKind, winner string, suppressed string) {
	t.Helper()
	for _, decision := range commit.Decisions {
		if decision.ScreenID != screenID {
			continue
		}
		if decision.Category != category || decision.Winner != winner {
			continue
		}
		if suppressed != "" {
			found := false
			for _, eventID := range decision.Suppressed {
				if eventID == suppressed {
					found = true
				}
			}
			if !found {
				t.Fatalf("decision did not suppress %s: %+v", suppressed, decision)
			}
		}
		return
	}
	t.Fatalf("missing decision window=%d screen=%s category=%s winner=%s", commit.Window, screenID, category, winner)
}

func findScreenCommit(t *testing.T, commit model.WindowCommit, screenID string) model.ScreenCommit {
	t.Helper()
	for _, screen := range commit.Screens {
		if screen.ScreenID == screenID {
			return screen
		}
	}
	t.Fatalf("screen %s missing from window %d", screenID, commit.Window)
	return model.ScreenCommit{}
}

func hasReplayPacket(screen model.ScreenCommit, sourceWindow int) bool {
	for _, packet := range screen.Packets {
		if packet.Replay && packet.SourceWindow == sourceWindow {
			return true
		}
	}
	return false
}

func assertKernelError(t *testing.T, err error, code model.Code) {
	t.Helper()
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) || kernelErr == nil {
		t.Fatalf("expected %s, got %T: %v", code, err, err)
	}
	if kernelErr.Code != code {
		t.Fatalf("code=%s want=%s (%v)", kernelErr.Code, code, err)
	}
}
