package mirror

import (
	"context"
	"path/filepath"
	"testing"
)

func TestThreePathsMatchTruth(t *testing.T) {
	sample := mustGenerate(t, 42)
	report, err := RunThreePaths(context.Background(), sample.Session, t.TempDir(), BudgetConfig{
		MaxConcurrency: 3, BufferPerWorker: 1, QueueBudget: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Matched {
		t.Fatalf("three paths differ: %v", report.FirstDiff)
	}
	for mode, result := range map[string]*RunResult{
		"realtime": report.Realtime, "resume": report.Resume, "replay": report.Replay,
	} {
		if err := CompareResultToTruth(result, sample.Truth); err != nil {
			t.Fatalf("%s truth check: %v", mode, err)
		}
		if result.FinalState.Session != string(SessionEnded) {
			t.Fatalf("%s final session state = %s", mode, result.FinalState.Session)
		}
	}
	if len(sample.Truth.Invalidations) == 0 {
		t.Fatal("expected at least one invalidated in-flight window in sample")
	}
}

func TestSingleScreenFailureIsIsolated(t *testing.T) {
	sample := mustGenerate(t, 42)
	s := cloneSession(sample.Session)
	for i := range s.Screens {
		if s.Screens[i].ID == "screen-alpha" {
			s.Screens[i].ScaleCapabilities = append(s.Screens[i].ScaleCapabilities, Scale150)
		}
	}
	s.Windows[0].Scale = Scale150
	if err := NormalizeSession(s); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), s, RunOptions{Mode: ModeReplay})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Windows) == 0 {
		t.Fatal("no windows committed")
	}
	first := result.Windows[0]
	success, failed := 0, 0
	for _, sc := range first.Screens {
		if sc.Failed {
			failed++
		} else {
			success++
		}
	}
	if success != 1 || failed != len(first.Screens)-1 {
		t.Fatalf("first window success=%d failed=%d screens=%d", success, failed, len(first.Screens))
	}
	if first.Status != WindowCommitted {
		t.Fatalf("first window status = %s", first.Status)
	}
	for _, w := range result.Windows[1:] {
		for _, sc := range w.Screens {
			if sc.ScreenID == "screen-bravo" {
				t.Fatalf("unrejoined failed screen %s polluted later window %s", sc.ScreenID, w.WindowID)
			}
		}
	}
}

func TestCompareRunResultsLocatesFirstMismatch(t *testing.T) {
	sample := mustGenerate(t, 1)
	result, err := Run(context.Background(), sample.Session, RunOptions{Mode: ModeReplay})
	if err != nil {
		t.Fatal(err)
	}
	other := *result
	other.Windows = append([]WindowCommit(nil), result.Windows...)
	other.Windows[0].Screens = append([]ScreenCommit(nil), result.Windows[0].Screens...)
	if len(other.Windows[0].Screens) == 0 {
		t.Fatal("test needs screens")
	}
	other.Windows[0].Screens[0].Mapping.Output.X++
	err = CompareRunResults(result, &other)
	if !IsCode(err, CodeDiff) {
		t.Fatalf("error = %v, want E_DIFF", err)
	}
	e, _ := AsError(err)
	if e.WindowID != result.Windows[0].WindowID || e.ScreenID != result.Windows[0].Screens[0].ScreenID || e.Field == "" {
		t.Fatalf("diff location = window=%q screen=%q field=%q", e.WindowID, e.ScreenID, e.Field)
	}
}

func TestResumeRejectsVersionMismatch(t *testing.T) {
	sample := mustGenerate(t, 42)
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), sample.Session, RunOptions{
		Mode: ModeRealtime, Checkpoint: store, StopAfter: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := cloneSession(sample.Session)
	s.AlgorithmVersion = "different-algorithm"
	if err := NormalizeSession(s); err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), s, RunOptions{Mode: ModeResume, Checkpoint: store, Resume: true})
	if !IsCode(err, CodeState) {
		t.Fatalf("error = %v, want E_STATE", err)
	}
}

func TestFailedScreenCanRetryAfterRenegotiation(t *testing.T) {
	s := &Session{
		ID: "retry", SourceVersion: SchemaV3, AlgorithmVersion: AlgorithmVersion,
		TickMillis: 10, MasterScreenID: "a",
		Domains: []Domain{{ID: "d"}},
		Screens: []Screen{{
			ID: "a", Name: "a", Domain: "d", Master: true, Width: 100, Height: 100,
			ScaleCapabilities: []Scale{100}, DefaultScale: 100,
			ChromaCapabilities: []Chroma{Chroma444}, DefaultChroma: Chroma444,
			MaxPacketBytes: 1000,
		}},
		Events: []Event{
			{ID: "join", Kind: EventJoin, ScreenID: "a", Domain: "d", AtTick: 0},
			{ID: "neg", Kind: EventNegotiate, ScreenID: "a", AtTick: 1},
			{ID: "retry", Kind: EventNegotiate, ScreenID: "a", AtTick: 15},
		},
		Windows: []Window{
			{ID: "w0", Index: 0, StartTick: 5, EndTick: 10,
				Region: Region{X: 0, Y: 0, W: 100, H: 100}, Scale: 150, Chroma: Chroma444, MaxPacketBytes: 1000},
			{ID: "w1", Index: 1, StartTick: 20, EndTick: 30,
				Region: Region{X: 0, Y: 0, W: 100, H: 100}, Scale: 100, Chroma: Chroma444, MaxPacketBytes: 1000},
		},
	}
	normalizeForTest(t, s)
	result, err := Run(context.Background(), s, RunOptions{Mode: ModeReplay})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Windows) != 2 {
		t.Fatalf("windows = %d", len(result.Windows))
	}
	if result.Windows[0].Status != WindowFailed || !result.Windows[0].Screens[0].Failed {
		t.Fatalf("first window did not fail isolated: %+v", result.Windows[0])
	}
	if result.Windows[1].Status != WindowCommitted || result.Windows[1].Screens[0].Failed {
		t.Fatalf("retry did not succeed: %+v", result.Windows[1])
	}
	generatedTruth, err := BuildGeneratorTruth(s)
	if err != nil {
		t.Fatal(err)
	}
	if path, av, bv, diff := FirstValueDifference(generatedTruth, BuildTruth(result)); diff {
		t.Fatalf("generator truth mismatch at %s: %v != %v", path, av, bv)
	}
}

func TestOutputIndependentOfWorkerBudget(t *testing.T) {
	sample := mustGenerate(t, 7)
	slow, err := Run(context.Background(), sample.Session, RunOptions{
		Mode: ModeReplay, Budget: BudgetConfig{MaxConcurrency: 1, BufferPerWorker: 1, QueueBudget: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	wide, err := Run(context.Background(), sample.Session, RunOptions{
		Mode: ModeReplay, Budget: BudgetConfig{MaxConcurrency: 8, BufferPerWorker: 4, QueueBudget: 16},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRunResults(slow, wide); err != nil {
		t.Fatalf("worker budget changed output: %v", err)
	}
}

func TestGeneratorTruthMatchesExecutedReplayTruth(t *testing.T) {
	sample := mustGenerate(t, 42)
	result, err := Run(context.Background(), sample.Session, RunOptions{Mode: ModeReplay})
	if err != nil {
		t.Fatal(err)
	}
	executed := BuildTruth(result)
	executed.Seed = sample.Seed
	if path, av, bv, diff := FirstValueDifference(sample.Truth, executed); diff {
		t.Fatalf("generator truth differs at %s: %v != %v", path, av, bv)
	}
}
