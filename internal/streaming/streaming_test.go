package streaming_test

import (
	"testing"

	"occlbench/internal/core"
	"occlbench/internal/culling"
	"occlbench/internal/generator"
	"occlbench/internal/streaming"
)

func testConfig() streaming.Config {
	return streaming.Config{
		WindowSize: 3,
		Budget:     streaming.Budget{Slots: 2, Memory: 50},
		CullParams: culling.DefaultPreciseParams(),
	}
}

func TestLegalAndIllegalTransitions(t *testing.T) {
	cases := []struct {
		from, to streaming.State
		legal    bool
	}{
		{streaming.StatePending, streaming.StateCulling, true},
		{streaming.StateCulling, streaming.StateLoading, true},
		{streaming.StateLoading, streaming.StateCommitted, true},
		{streaming.StateLoading, streaming.StateFailed, true},
		{streaming.StateFailed, streaming.StateCulling, true},
		{streaming.StateCommitted, streaming.StateLoading, false},
		{streaming.StatePending, streaming.StateCommitted, false},
	}
	for _, tc := range cases {
		got, err := streaming.Transition(tc.from, tc.to)
		if tc.legal {
			if err != nil || got != tc.to {
				t.Errorf("%s->%s should be legal", tc.from, tc.to)
			}
		} else {
			if core.CodeOf(err) != core.EState {
				t.Errorf("%s->%s want E_STATE got %v", tc.from, tc.to, err)
			}
			if got != tc.from {
				t.Errorf("illegal transition must keep original state %s, got %s", tc.from, got)
			}
		}
	}
}

func TestBudgetValidation(t *testing.T) {
	if err := (streaming.Budget{Slots: 0, Memory: 10}).Validate(); core.CodeOf(err) != core.EBudget {
		t.Fatalf("slots=0 want E_BUDGET got %v", err)
	}
	if err := (streaming.Budget{Slots: 2, Memory: -1}).Validate(); core.CodeOf(err) != core.EBudget {
		t.Fatalf("memory=-1 want E_BUDGET got %v", err)
	}
}

func TestRunAllProducesCommittedWindows(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	eng, err := streaming.NewEngine(w, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	windows, err := eng.RunAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 4 { // 12 采样 / 窗口 3
		t.Fatalf("want 4 windows got %d", len(windows))
	}
	committed, total := eng.Progress()
	if committed != total || total != 4 {
		t.Fatalf("progress = %d/%d", committed, total)
	}
	for _, win := range windows {
		if win.State != streaming.StateCommitted || !win.Frozen {
			t.Fatalf("window %d not committed/frozen: %s", win.Index, win.State)
		}
		if win.ContentHash == "" {
			t.Fatalf("window %d missing content hash", win.Index)
		}
	}
}

func TestInvisibleChunkNeverGrabsSlot(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	sched, err := streaming.NewScheduler(w, streaming.Budget{Slots: 1, Memory: 1})
	if err != nil {
		t.Fatal(err)
	}
	visible := w.GroundTruth.Visible[5]
	cam := w.Trajectory.Samples[5].Pos
	plan := sched.Plan(0, []int{}, visible, cam)
	for _, a := range plan.Loads {
		found := false
		for _, id := range visible {
			if id == a.ChunkID {
				found = true
			}
		}
		if !found {
			t.Fatalf("invisible chunk %d grabbed a slot", a.ChunkID)
		}
	}
	// 槽位=1 时多余可见分块必须进入延迟队列而非被丢弃。
	if len(visible) > 1 && len(plan.Delayed) == 0 {
		t.Fatal("expected delayed chunks under slot=1")
	}
}

func TestDeterministicScheduleAcrossRuns(t *testing.T) {
	w := generator.Generate(generator.DefaultConfig())
	cfg := testConfig()
	run := func() []*streaming.Window {
		eng, _ := streaming.NewEngine(w, cfg)
		ws, err := eng.RunAll()
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	a, b := run(), run()
	for i := range a {
		if !equalSlice(a[i].LoadList, b[i].LoadList) || !equalSlice(a[i].Resident, b[i].Resident) {
			t.Fatalf("window %d schedule not deterministic", i)
		}
	}
}

func equalSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
