package dialoguecal

import (
	"context"
	"testing"
)

func TestFullRunMatchesGeneratedTruth(t *testing.T) {
	g := mustGenerate(t, 123)
	r := mustRun(t, NewEngine(nil), g.Project, ModeFull)
	if err := CompareTruth(g.Truth, r.Intervals, g.Project.Tolerance); err != nil {
		t.Fatalf("truth comparison: %v", err)
	}
	dev, err := TruthDeviation(g.Truth, r.Intervals)
	if err != nil {
		t.Fatal(err)
	}
	if dev > g.Project.Tolerance {
		t.Fatalf("loudness deviation %d exceeds tolerance %d", dev, g.Project.Tolerance)
	}
	if dev != 0 {
		t.Fatalf("fixed-point kernel should match generated truth exactly, got %d", dev)
	}
	// Tightening tolerance cannot make a non-decreasing observed deviation
	// smaller; processing does not consult tolerance.
	if err := CompareTruth(g.Truth, r.Intervals, g.Project.Tolerance-100); err != nil {
		t.Fatalf("tightened truth comparison: %v", err)
	}
	tightDev, err := TruthDeviation(g.Truth, r.Intervals)
	if err != nil {
		t.Fatal(err)
	}
	if tightDev < dev {
		t.Fatalf("tightening tolerance reduced observed deviation: %d < %d", tightDev, dev)
	}
}

func TestWorkerPoolDeterminismAndQueueBudget(t *testing.T) {
	g := mustGenerate(t, 321)
	intervals, err := BuildIntervals(g.Project)
	if err != nil {
		t.Fatal(err)
	}
	f1, events1, err := ExtractFrames(context.Background(), g.Project, intervals, ScheduleOptions{
		MaxParallelism: 1, WorkerBufferSamples: 2048, QueueBudget: 1,
	}, Injection{})
	if err != nil {
		t.Fatal(err)
	}
	f2, _, err := ExtractFrames(context.Background(), g.Project, intervals, ScheduleOptions{
		MaxParallelism: 4, WorkerBufferSamples: 2048, QueueBudget: 1,
	}, Injection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(f1) != len(f2) || FeatureDigest(f1) != FeatureDigest(f2) {
		t.Fatalf("worker count changed features: n=%d/%d digest=%s/%s",
			len(f1), len(f2), FeatureDigest(f1), FeatureDigest(f2))
	}
	if len(events1) == 0 {
		t.Fatal("bounded queue did not record any queue-full wait")
	}
	for _, ev := range events1 {
		if ev.Reason != "queue_budget_exhausted" || ev.IntervalID == "" {
			t.Fatalf("malformed queue event: %+v", ev)
		}
	}
}

func TestFullRealtimeAndResumeAreFrameIdentical(t *testing.T) {
	g := mustGenerate(t, 55)
	store, _ := mustStore(t)
	engine := NewEngine(store)
	full := mustRun(t, engine, g.Project, ModeFull)
	resume := mustRun(t, engine, g.Project, ModeResume)
	if len(resume.ReusedIntervals) != len(full.Intervals) {
		t.Fatalf("resume reused %d/%d intervals", len(resume.ReusedIntervals), len(full.Intervals))
	}
	if len(resume.PhysicalCommitOrder) != 0 {
		t.Fatalf("fully reused resume wrote %d intervals", len(resume.PhysicalCommitOrder))
	}
	if err := CompareRunResults(full, resume); err != nil {
		t.Fatalf("full/resume mismatch: %v", err)
	}
	realtime := mustRun(t, engine, g.Project, ModeRealTime)
	if err := CompareRunResults(full, realtime); err != nil {
		t.Fatalf("full/realtime mismatch: %v", err)
	}
}

func TestCheckpointRecoveryAfterStopAndCancel(t *testing.T) {
	g := mustGenerate(t, 77)
	store, _ := mustStore(t)
	engine := NewEngine(store)
	baseline := mustRun(t, NewEngine(nil), g.Project, ModeFull)

	partial, err := engine.Run(context.Background(), g.Project, RunOptions{
		Mode: ModeFull, StopAfterCommits: 4,
	})
	ke := requireCode(t, err, CodeCancel)
	if ke.SegmentID == "" || len(partial.PhysicalCommitOrder) != 4 {
		t.Fatalf("stop boundary incomplete: %+v", partial.PhysicalCommitOrder)
	}
	recovered := mustRun(t, engine, g.Project, ModeResume)
	if len(recovered.ReusedIntervals) != 4 {
		t.Fatalf("recovered %d intervals, want 4", len(recovered.ReusedIntervals))
	}
	if err := CompareRunResults(baseline, recovered); err != nil {
		t.Fatalf("recovered run differs: %v", err)
	}

	store2, _ := mustStore(t)
	engine2 := NewEngine(store2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled, err := engine2.Run(ctx, g.Project, RunOptions{Mode: ModeFull})
	requireCode(t, err, CodeCancel)
	if len(canceled.PhysicalCommitOrder) != 0 {
		t.Fatalf("canceled run committed %d intervals", len(canceled.PhysicalCommitOrder))
	}
	afterCancel := mustRun(t, engine2, g.Project, ModeResume)
	if err := CompareRunResults(baseline, afterCancel); err != nil {
		t.Fatalf("resume after cancel differs: %v", err)
	}
}

func TestPreciseGainAndTargetInvalidation(t *testing.T) {
	g := mustGenerate(t, 88)
	store, _ := mustStore(t)
	engine := NewEngine(store)
	mustRun(t, engine, g.Project, ModeFull)

	gainEdit := ProjectEdit{Kind: EditSegmentGain, SegmentID: "seg-01-01", DialogueID: "dlg-01", IntValue: 2500}
	editedGain, err := ApplyEdit(g.Project, gainEdit)
	if err != nil {
		t.Fatal(err)
	}
	gainRun, err := engine.Run(context.Background(), editedGain, RunOptions{
		Mode: ModeResume, PreviousProject: g.Project,
	})
	if err != nil {
		t.Fatalf("gain edit run: %v", err)
	}
	if gainRun.Invalidation == nil || len(gainRun.Invalidation.Items) != 1 {
		t.Fatalf("gain invalidation should affect one interval: %+v", gainRun.Invalidation)
	}
	item := gainRun.Invalidation.Items[0]
	if item.SegmentID != "seg-01-01" || item.StartFrame != 0 || item.EndFrame != 7 ||
		item.Reason != "segment_gain_changed" || !item.FeatureReusable {
		t.Fatalf("wrong gain invalidation item: %+v", item)
	}
	if len(gainRun.ReusedIntervals) != len(gainRun.Intervals)-1 {
		t.Fatalf("unaffected intervals not reused: %d/%d", len(gainRun.ReusedIntervals), len(gainRun.Intervals))
	}
	baselineGain := mustRun(t, NewEngine(nil), editedGain, ModeFull)
	if err := CompareRunResults(baselineGain, gainRun); err != nil {
		t.Fatalf("gain incremental run differs from full: %v", err)
	}

	store2, _ := mustStore(t)
	engine2 := NewEngine(store2)
	mustRun(t, engine2, g.Project, ModeFull)
	targetEdit := ProjectEdit{Kind: EditTarget, IntValue: g.Project.TargetLoudness + 1000}
	editedTarget, err := ApplyEdit(g.Project, targetEdit)
	if err != nil {
		t.Fatal(err)
	}
	targetRun, err := engine2.Run(context.Background(), editedTarget, RunOptions{
		Mode: ModeResume, PreviousProject: g.Project,
	})
	if err != nil {
		t.Fatalf("target edit run: %v", err)
	}
	if targetRun.Invalidation == nil || len(targetRun.Invalidation.Items) != len(targetRun.Intervals) {
		t.Fatalf("target should invalidate every calibration interval: %+v", targetRun.Invalidation)
	}
	for _, item := range targetRun.Invalidation.Items {
		if item.Reason != "target_loudness_changed" || !item.FeatureReusable {
			t.Fatalf("target invalidation was not calibration-only: %+v", item)
		}
	}
	if len(targetRun.ReusedFeatureIntervals) != len(targetRun.Intervals) {
		t.Fatalf("target edit should reuse persisted features: %d/%d", len(targetRun.ReusedFeatureIntervals), len(targetRun.Intervals))
	}
	for _, iv := range targetRun.Intervals {
		if !iv.ReusedFeatures {
			t.Fatalf("target edit interval did not expose feature reuse: %s", iv.IntervalID)
		}
	}
	baselineTarget := mustRun(t, NewEngine(nil), editedTarget, ModeFull)
	if err := CompareRunResults(baselineTarget, targetRun); err != nil {
		t.Fatalf("target incremental run differs from full: %v", err)
	}
}

func TestFrameRateChangeRequeuesAllIntervals(t *testing.T) {
	g := mustGenerate(t, 909)
	store, _ := mustStore(t)
	engine := NewEngine(store)
	mustRun(t, engine, g.Project, ModeFull)
	edited, err := ApplyEdit(g.Project, ProjectEdit{Kind: EditFrameRate, IntValue: 60})
	if err != nil {
		t.Fatal(err)
	}
	r, err := engine.Run(context.Background(), edited, RunOptions{
		Mode: ModeResume, PreviousProject: g.Project,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Invalidation == nil || len(r.Invalidation.Items) == 0 {
		t.Fatal("frame-rate edit produced no invalidation")
	}
	for _, item := range r.Invalidation.Items {
		if item.FeatureReusable {
			t.Fatalf("frame-rate edit incorrectly reused features: %+v", item)
		}
	}
	baseline := mustRun(t, NewEngine(nil), edited, ModeFull)
	if err := CompareRunResults(baseline, r); err != nil {
		t.Fatalf("frame-rate incremental run differs: %v", err)
	}
}

func TestInvalidationReportIsStableAcrossRunModes(t *testing.T) {
	g := mustGenerate(t, 919)
	store, _ := mustStore(t)
	engine := NewEngine(store)
	mustRun(t, engine, g.Project, ModeFull)
	edited, err := ApplyEdit(g.Project, ProjectEdit{
		Kind: EditTarget, IntValue: g.Project.TargetLoudness + 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	resume, err := engine.Run(context.Background(), edited, RunOptions{
		Mode: ModeResume, PreviousProject: g.Project,
	})
	if err != nil {
		t.Fatal(err)
	}
	full, err := engine.Run(context.Background(), edited, RunOptions{
		Mode: ModeFull, PreviousProject: g.Project,
	})
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := engine.Run(context.Background(), edited, RunOptions{
		Mode: ModeRealTime, PreviousProject: g.Project,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []*RunResult{full, realtime} {
		if other.Invalidation == nil || resume.Invalidation == nil ||
			len(other.Invalidation.Items) != len(resume.Invalidation.Items) {
			t.Fatal("missing or differently sized invalidation report")
		}
		for i := range resume.Invalidation.Items {
			a, b := resume.Invalidation.Items[i], other.Invalidation.Items[i]
			if a.IntervalID != b.IntervalID || a.StartFrame != b.StartFrame || a.EndFrame != b.EndFrame ||
				a.Reason != b.Reason || a.FeatureReusable != b.FeatureReusable {
				t.Fatalf("mode-dependent invalidation: %+v != %+v", a, b)
			}
		}
	}
}

func TestRunDiffLocatesFirstFrame(t *testing.T) {
	g := mustGenerate(t, 101)
	a := mustRun(t, NewEngine(nil), g.Project, ModeFull)
	b := mustRun(t, NewEngine(nil), g.Project, ModeFull)
	b.Intervals[2].Features[1].Energy++
	err := CompareRunResults(a, b)
	ke := requireCode(t, err, CodeDiff)
	if ke.FirstSegmentID == "" || !ke.HasFirstFrame {
		t.Fatalf("E_DIFF locator missing: %+v", ke)
	}
}

func TestResumeRejectsCheckpointFromDifferentSourceVersion(t *testing.T) {
	g := mustGenerate(t, 202)
	store, _ := mustStore(t)
	engine := NewEngine(store)
	mustRun(t, engine, g.Project, ModeFull)
	v1, err := LoadProject(g.Docs[VersionV1])
	if err != nil {
		t.Fatal(err)
	}
	r, err := engine.Run(context.Background(), v1, RunOptions{Mode: ModeResume})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ReusedIntervals) != 0 || len(r.ReusedFeatureIntervals) != 0 {
		t.Fatalf("resume reused checkpoints across schema versions: %+v", r.ReusedIntervals)
	}
	if len(r.PhysicalCommitOrder) != len(r.Intervals) {
		t.Fatal("version-mismatched intervals were not rebuilt")
	}
}
