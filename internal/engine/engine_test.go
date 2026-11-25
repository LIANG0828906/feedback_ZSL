package engine_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"ms-workbench/internal/apperr"
	"ms-workbench/internal/engine"
	"ms-workbench/internal/samples"
	"ms-workbench/internal/scene"
	"ms-workbench/internal/store"

	_ "modernc.org/sqlite"
)

func loadScene(t *testing.T) *scene.Scene {
	t.Helper()
	pkg, err := samples.Generate(samples.Options{Seed: 20261125, Width: 64, Height: 48}, "engine", "Engine Test")
	if err != nil {
		t.Fatal(err)
	}
	s, err := scene.Decode(pkg.Files[3])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newEngine(t *testing.T, s *scene.Scene, dbPath string, cfg engine.Config) (*engine.Engine, *store.Checkpoint) {
	t.Helper()
	cp, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(s, cp, cfg)
	if err != nil {
		cp.Close()
		t.Fatal(err)
	}
	return e, cp
}

func runComplete(t *testing.T, e *engine.Engine, mode engine.RunMode) engine.RunReport {
	t.Helper()
	report, err := e.Run(context.Background(), engine.RunOptions{Mode: mode})
	if err != nil {
		t.Fatalf("run %s: %v", mode, err)
	}
	if !report.Complete {
		t.Fatalf("%s run incomplete: %+v", mode, report)
	}
	return report
}

func TestStateMachineRejectsIllegalTransition(t *testing.T) {
	sm := engine.NewStateMachine(engine.Pending)
	err := sm.Transition(engine.Committed)
	if !apperr.IsCode(err, apperr.ESTATE) {
		t.Fatalf("error = %v", err)
	}
	if sm.State() != engine.Pending {
		t.Fatalf("state changed to %s after illegal transition", sm.State())
	}
}

func TestFullRunAccuracyAndSerialCommitSequence(t *testing.T) {
	s := loadScene(t)
	cfg := engine.DefaultConfig()
	e, cp := newEngine(t, s, filepath.Join(t.TempDir(), "full.db"), cfg)
	defer cp.Close()
	report := runComplete(t, e, engine.ModeFull)
	for i, id := range report.CommitSequence {
		if id != i {
			t.Fatalf("commit sequence = %v", report.CommitSequence)
		}
	}
	events, err := cp.EventSequence()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != report.Total {
		t.Fatalf("event count %d total %d", len(events), report.Total)
	}
	for i := range events {
		if events[i] != i {
			t.Fatalf("durable commit events are not ascending: %v", events)
		}
	}
	comp := e.Composite()
	if comp.PeakRadiance <= 0 || len(comp.OutOfGamut) == 0 {
		t.Fatalf("missing peak/gamut observability: peak=%f oog=%d", comp.PeakRadiance, len(comp.OutOfGamut))
	}
	measurements, err := e.PatchMeasurements()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range measurements {
		if !m.Pass {
			t.Fatalf("patch %s failed: %+v", m.Name, m)
		}
	}
}

func TestThreePathsPixelIdenticalAndResumeFromPartialSnapshot(t *testing.T) {
	s := loadScene(t)
	cfg := engine.DefaultConfig()
	cfg.MaxWorkers = 3

	live, liveCP := newEngine(t, s, filepath.Join(t.TempDir(), "live.db"), cfg)
	liveReport := runComplete(t, live, engine.ModeLive)
	liveComp := live.Composite()
	liveCP.Close()

	partialPath := filepath.Join(t.TempDir(), "resume.db")
	part, partCP := newEngine(t, s, partialPath, cfg)
	first, err := part.Run(context.Background(), engine.RunOptions{Mode: engine.ModeLive, MaxNewCommits: 4})
	if err != nil {
		t.Fatal(err)
	}
	if first.Complete || first.NewCommits != 4 {
		t.Fatalf("expected interrupted partial pass, got %+v", first)
	}
	if err := partCP.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, resumedCP := newEngine(t, s, partialPath, cfg)
	if !resumed.LoadedFromCheckpoint() {
		t.Fatalf("resume did not load checkpoint: %s", resumed.SnapshotResetReason())
	}
	resumeReport := runComplete(t, resumed, engine.ModeResume)
	resumedComp := resumed.Composite()
	resumedCP.Close()

	full, fullCP := newEngine(t, s, filepath.Join(t.TempDir(), "full.db"), cfg)
	fullReport := runComplete(t, full, engine.ModeFull)
	fullComp := full.Composite()
	fullCP.Close()

	if !sameSequence(liveReport.CommitSequence, fullReport.CommitSequence) ||
		!sameSequence(resumeReport.CommitSequence, fullReport.CommitSequence) {
		t.Fatalf("commit sequences differ: live=%v resume=%v full=%v", liveReport.CommitSequence, resumeReport.CommitSequence, fullReport.CommitSequence)
	}
	for name, comp := range map[string]*engine.Composite{"live": liveComp, "resume": resumedComp} {
		diff := engine.CompareComposites(comp, fullComp, cfg)
		if !diff.Equal {
			t.Fatalf("%s differs from full at first pixel: %+v", name, diff.FirstDifference)
		}
		if diff.MaxAbsDelta != 0 || !diff.PeakEqual || !diff.OutOfGamutEqual {
			t.Fatalf("%s difference report: %+v", name, diff)
		}
	}
}

func TestIncrementalInvalidationEqualsFullRecompute(t *testing.T) {
	s := loadScene(t)
	cfg := engine.DefaultConfig()
	incremental, cp := newEngine(t, s, filepath.Join(t.TempDir(), "incremental.db"), cfg)
	runComplete(t, incremental, engine.ModeFull)

	next := s.Settings
	next.GamutIntent = "perceptual"
	report, err := incremental.SetSettings(next)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Segments) == 0 {
		t.Fatal("missing invalidation list")
	}
	for _, item := range report.Segments {
		if item.Stage != "chroma" || item.Reused {
			t.Fatalf("gamut edit should invalidate chroma only: %+v", item)
		}
	}
	runComplete(t, incremental, engine.ModeResume)
	incrementalComp := incremental.Composite()
	if len(incrementalComp.CommitSequence) != incrementalComp.Total {
		t.Fatalf("incremental commit sequence incomplete: %v", incrementalComp.CommitSequence)
	}
	cp.Close()

	s2 := loadScene(t)
	s2.Settings = next
	full, fullCP := newEngine(t, s2, filepath.Join(t.TempDir(), "newfull.db"), cfg)
	defer fullCP.Close()
	runComplete(t, full, engine.ModeFull)
	diff := engine.CompareComposites(incrementalComp, full.Composite(), cfg)
	if !diff.Equal {
		t.Fatalf("incremental/full differ: %+v", diff)
	}
	if !sameSequence(incrementalComp.CommitSequence, full.Composite().CommitSequence) {
		t.Fatalf("commit sequence differs: %v vs %v", incrementalComp.CommitSequence, full.Composite().CommitSequence)
	}
}

func TestPartialBandReplacementOnlyInvalidatesAffectedSegments(t *testing.T) {
	s := loadScene(t)
	cfg := engine.DefaultConfig()
	incremental, cp := newEngine(t, s, filepath.Join(t.TempDir(), "partial.db"), cfg)
	runComplete(t, incremental, engine.ModeFull)

	replacement := s.Bands[1].Clone()
	// Touch a single pixel in segment 5's top-left area.
	idx := 5*5*s.Width + 5*5
	replacement.Exposures[0].Codes[idx] ^= 1
	report, err := incremental.ReplaceBand(s.Bands[1].ID, replacement)
	if err != nil {
		t.Fatal(err)
	}
	invalidated := 0
	reused := 0
	for _, item := range report.Segments {
		if item.Stage == "fusion+chroma" {
			invalidated++
		}
		if item.Reused {
			reused++
		}
	}
	if invalidated == 0 || invalidated >= len(report.Segments) || reused == 0 {
		t.Fatalf("partial edit propagation too broad/narrow: invalidated=%d reused=%d report=%+v", invalidated, reused, report)
	}
	runComplete(t, incremental, engine.ModeResume)
	incrementalComp := incremental.Composite()
	cp.Close()

	s2 := loadScene(t)
	s2.Bands[1] = replacement
	full, fullCP := newEngine(t, s2, filepath.Join(t.TempDir(), "partial-full.db"), cfg)
	defer fullCP.Close()
	runComplete(t, full, engine.ModeFull)
	diff := engine.CompareComposites(incrementalComp, full.Composite(), cfg)
	if !diff.Equal {
		t.Fatalf("partial incremental differs: %+v", diff)
	}
}

func TestPartialEditSnapshotSurvivesRestartAndReusesUnaffectedRows(t *testing.T) {
	s := loadScene(t)
	cfg := engine.DefaultConfig()
	dbPath := filepath.Join(t.TempDir(), "partial-restart.db")
	incremental, cp := newEngine(t, s, dbPath, cfg)
	runComplete(t, incremental, engine.ModeFull)

	replacement := s.Bands[2].Clone()
	idx := 6*5*s.Width + 6*5
	replacement.Exposures[2].Codes[idx] = (replacement.Exposures[2].Codes[idx] + 29) % 4096
	report, err := incremental.ReplaceBand(s.Bands[2].ID, replacement)
	if err != nil {
		t.Fatal(err)
	}
	reusedIDs := map[int]bool{}
	for _, item := range report.Segments {
		if item.Reused {
			reusedIDs[item.SegmentID] = true
		}
	}
	editedScene := incremental.Scene()
	cp.Close()

	resumed, cp2 := newEngine(t, editedScene, dbPath, cfg)
	if !resumed.LoadedFromCheckpoint() {
		t.Fatalf("edited partial snapshot did not load: %s", resumed.SnapshotResetReason())
	}
	view := resumed.View()
	for _, seg := range view.Segments {
		if seg.State == engine.Committed && !reusedIDs[seg.ID] {
			t.Fatalf("segment %d was restored although it was invalidated", seg.ID)
		}
	}
	if view.Committed != len(reusedIDs) {
		t.Fatalf("restored committed=%d, expected reuse=%d", view.Committed, len(reusedIDs))
	}
	runComplete(t, resumed, engine.ModeResume)
	got := resumed.Composite()
	cp2.Close()

	s2 := loadScene(t)
	s2.Bands[2] = replacement
	full, fullCP := newEngine(t, s2, filepath.Join(t.TempDir(), "partial-restart-full.db"), cfg)
	defer fullCP.Close()
	runComplete(t, full, engine.ModeFull)
	diff := engine.CompareComposites(got, full.Composite(), cfg)
	if !diff.Equal {
		t.Fatalf("restart/incremental differs from full: %+v", diff)
	}
}

func TestCorruptHashInvalidatesEntireSnapshot(t *testing.T) {
	s := loadScene(t)
	dbPath := filepath.Join(t.TempDir(), "corrupt.db")
	cfg := engine.DefaultConfig()
	e, cp := newEngine(t, s, dbPath, cfg)
	runComplete(t, e, engine.ModeFull)
	cp.Close()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE segments SET content_hash='corrupt' WHERE segment_id=0`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cp2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cp2.Close()
	e2, err := engine.New(s, cp2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if e2.LoadedFromCheckpoint() {
		t.Fatal("corrupt snapshot was partially reused")
	}
	if e2.SnapshotResetReason() == "" {
		t.Fatal("missing snapshot reset reason")
	}
	count, err := cp2.SegmentCount()
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected entire snapshot discarded, still have %d rows", count)
	}
	for _, seg := range e2.View().Segments {
		if seg.State != engine.Pending {
			t.Fatalf("segment %d state = %s", seg.ID, seg.State)
		}
	}
}

func TestVersionMismatchDiscardsWholeSnapshot(t *testing.T) {
	s := loadScene(t)
	dbPath := filepath.Join(t.TempDir(), "version.db")
	cfg := engine.DefaultConfig()
	e, cp := newEngine(t, s, dbPath, cfg)
	runComplete(t, e, engine.ModeFull)
	cp.Close()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE snapshot SET param_version='different'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cp2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cp2.Close()
	e2, err := engine.New(s, cp2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if e2.LoadedFromCheckpoint() {
		t.Fatal("version-mismatched snapshot was reused")
	}
	count, _ := cp2.SegmentCount()
	if count != 0 {
		t.Fatalf("expected snapshot rows deleted, got %d", count)
	}
}

func TestVerifierComparesAllThreePaths(t *testing.T) {
	s := loadScene(t)
	cfg := engine.DefaultConfig()
	report, err := engine.Verify(context.Background(), s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Equal || !report.CommitSequencesEqual {
		t.Fatalf("verification failed: %+v", report)
	}
	if !report.Resume.ResumedFromStore {
		t.Fatal("resume verification did not actually restore a persisted snapshot")
	}
	if report.PeakRadianceDelta > s.Truth.PatchTolerance {
		t.Fatalf("peak radiance delta %.8f > tolerance %.8f", report.PeakRadianceDelta, s.Truth.PatchTolerance)
	}
	for _, p := range report.Patches {
		if !p.Pass {
			t.Fatalf("patch %s failed", p.Name)
		}
	}
}

func TestPauseAtBatchBoundaryThenResume(t *testing.T) {
	s := loadScene(t)
	cfg := engine.DefaultConfig()
	cfg.MaxWorkers = 1
	cfg.ChunkDelayMS = 5
	e, cp := newEngine(t, s, filepath.Join(t.TempDir(), "pause.db"), cfg)
	defer cp.Close()
	done := make(chan engine.RunReport, 1)
	go func() {
		report, err := e.Run(context.Background(), engine.RunOptions{Mode: engine.ModeFull})
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- report
	}()
	time.Sleep(12 * time.Millisecond)
	e.Pause()
	report := <-done
	if report.Complete {
		t.Fatal("pause did not stop the pass at a batch boundary")
	}
	if e.View().Paused != true {
		t.Fatal("engine did not report paused state")
	}
	final, err := e.Run(context.Background(), engine.RunOptions{Mode: engine.ModeResume})
	if err != nil {
		t.Fatal(err)
	}
	if !final.Complete {
		t.Fatalf("resume incomplete: %+v", final)
	}
	full, fullCP := newEngine(t, s, filepath.Join(t.TempDir(), "pause-full.db"), cfg)
	defer fullCP.Close()
	runComplete(t, full, engine.ModeFull)
	if diff := engine.CompareComposites(e.Composite(), full.Composite(), cfg); !diff.Equal {
		t.Fatalf("pause/resume differs from full: %+v", diff)
	}
}

func sameSequence(a, b []int) bool {
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
