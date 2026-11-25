package vpcompose_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"vpcompose"
	"vpcompose/sample"
)

func loadProjectForTest(t *testing.T, seed int64) *vpcompose.Project {
	t.Helper()
	generated, err := sample.Generate(seed)
	if err != nil {
		t.Fatal(err)
	}
	project, err := vpcompose.LoadProject(generated.Files["project-v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func TestWindowStateMachineRejectsIllegalTransitions(t *testing.T) {
	project := loadProjectForTest(t, 20)
	engine, err := vpcompose.NewEngine(project, nil, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	window := engine.Windows()[0]
	if window.State != vpcompose.WindowPending {
		t.Fatalf("initial state = %s", window.State)
	}
	err = window.Commit(&vpcompose.WindowResult{WindowIndex: 0})
	if !vpcompose.IsCode(err, vpcompose.CodeState) {
		t.Fatalf("pending -> committed error = %v", err)
	}
	if window.State != vpcompose.WindowPending {
		t.Fatalf("illegal transition changed state to %s", window.State)
	}
	if err := window.Start(); err != nil {
		t.Fatal(err)
	}
	if err := window.Fail(context.Canceled); err != nil {
		t.Fatal(err)
	}
	if window.State != vpcompose.WindowFailed {
		t.Fatalf("state after failure = %s", window.State)
	}
	if err := window.Start(); err != nil {
		t.Fatalf("failed -> compositing retry: %v", err)
	}
	if err := window.Commit(&vpcompose.WindowResult{WindowIndex: 0}); err != nil {
		t.Fatal(err)
	}
	if window.State != vpcompose.WindowCommitted {
		t.Fatalf("final state = %s", window.State)
	}
}

func TestEditInvalidatesOnlyAffectedUncommittedWindows(t *testing.T) {
	project := loadProjectForTest(t, 21)
	engine, err := vpcompose.NewEngine(project, nil, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ComposeWindow(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	window2KeyBefore := engine.Dependencies().WindowKeys[2]
	first, ok := engine.ResultForWindow(0)
	if !ok {
		t.Fatal("window 0 was not committed")
	}
	originalDigest := first.Digest
	visible := false
	report, err := engine.EditLayer(vpcompose.LayerEdit{LayerID: "beauty-glow", Visible: &visible})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.WindowIDs(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("invalidated windows = %v, want [1]", got)
	}
	if got := engine.Dependencies().WindowKeys[2]; got != window2KeyBefore {
		t.Fatal("unaffected window 2 cache key changed")
	}
	if len(report.Entries) != 1 || len(report.Entries[0].LayerIDs) != 1 || report.Entries[0].LayerIDs[0] != "beauty-glow" {
		t.Fatalf("invalidation entry = %+v", report.Entries)
	}
	run, err := engine.Run(context.Background(), vpcompose.ModeRealtime)
	if err != nil {
		t.Fatal(err)
	}
	if run.Progress.Committed != 3 || run.Progress.Total != 3 {
		t.Fatalf("progress = %+v", run.Progress)
	}
	firstAfter, _ := engine.ResultForWindow(0)
	if firstAfter.Digest != originalDigest {
		t.Fatal("committed window 0 was recomputed or mutated")
	}
	if firstAfter != first {
		t.Fatal("committed result pointer was replaced")
	}
}

func TestTimebaseEditInvalidatesEveryUncommittedWindow(t *testing.T) {
	project := loadProjectForTest(t, 23)
	engine, err := vpcompose.NewEngine(project, nil, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ComposeWindow(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	report, err := engine.EditTimebase(vpcompose.Timebase{Unit: "tick", TickRate: 48000, Rounding: vpcompose.RoundHalfUp})
	if err != nil {
		t.Fatal(err)
	}
	if got := report.WindowIDs(); !equalInts(got, []int{1, 2}) {
		t.Fatalf("timebase invalidation = %v", got)
	}
	for _, entry := range report.Entries {
		if entry.Reason != "timebase" || len(entry.LayerIDs) != 4 {
			t.Fatalf("timebase entry = %+v", entry)
		}
	}
}

func TestFaultIsolationRetryAndQueueTelemetry(t *testing.T) {
	project := loadProjectForTest(t, 22)
	var fail atomic.Bool
	fail.Store(true)
	engine, err := vpcompose.NewEngine(project, nil, vpcompose.EngineOptions{
		Workers: 2,
		FaultInjector: func(window int) error {
			if window == 1 && fail.Load() {
				return context.DeadlineExceeded
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := engine.Run(context.Background(), vpcompose.ModeRealtime)
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(report.Failed, []int{1}) || !equalInts(report.Committed, []int{0, 2}) {
		t.Fatalf("fault report committed=%v failed=%v", report.Committed, report.Failed)
	}
	if state, _ := engine.WindowState(1); state != vpcompose.WindowFailed {
		t.Fatalf("window 1 state = %s", state)
	}
	if state, _ := engine.WindowState(2); state != vpcompose.WindowCommitted {
		t.Fatalf("window 2 was polluted: %s", state)
	}
	fail.Store(false)
	retry, err := engine.RetryFailed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if retry.Progress.Committed != 3 || len(retry.Failed) != 0 {
		t.Fatalf("retry report = %+v", retry.Progress)
	}

	queueEngine, err := vpcompose.NewEngine(project, nil, vpcompose.EngineOptions{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	queueReport, err := queueEngine.Run(context.Background(), vpcompose.ModeRealtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(queueReport.QueueEvents) == 0 {
		t.Fatal("expected queue telemetry under a one-worker budget")
	}
	for _, event := range queueReport.QueueEvents {
		if event.Reason == "" || event.Wait <= 0 {
			t.Fatalf("incomplete queue event: %+v", event)
		}
	}

	budgetEngine, err := vpcompose.NewEngine(project, nil, vpcompose.EngineOptions{
		Workers: 2, MaxPixelsPerWorker: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	budgetReport, err := budgetEngine.Run(context.Background(), vpcompose.ModeRealtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(budgetReport.Failed) != 3 {
		t.Fatalf("budget failures = %v", budgetReport.Failed)
	}
	for _, message := range budgetReport.Errors {
		if !strings.Contains(message, string(vpcompose.CodeBudget)) {
			t.Fatalf("budget error = %s", message)
		}
	}
}

func equalInts(a, b []int) bool {
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
