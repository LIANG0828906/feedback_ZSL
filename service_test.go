package ocrflow

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRunVerificationAcceptance(t *testing.T) {
	rep, err := RunVerification(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		for _, c := range rep.Checks {
			if !c.OK {
				t.Errorf("check %s failed: %s (%s)", c.Name, c.Detail, c.Code)
			}
		}
		t.Fatal("acceptance verification failed")
	}
}

func TestSchedulerBlocksAndRecordsBudgetWait(t *testing.T) {
	cfg := BudgetConfig{Workers: 1, PerWorkerPixelBytes: 1024, MaxQueuedTasks: 1, MaxQueuedPixelBytes: 4096}
	sched, err := NewScheduler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	h1, err := sched.Submit(Task{ID: "t1", Kind: "test", PixelBytes: 10, Run: func() error { <-release; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return sched.Snapshot().ActiveWorkers == 1 })

	h2, err := sched.Submit(Task{ID: "t2", Kind: "test", PixelBytes: 10, Run: func() error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return sched.Snapshot().QueuedTasks == 1 })

	type submitResult struct {
		h   *TaskHandle
		err error
	}
	resultCh := make(chan submitResult, 1)
	go func() {
		h, err := sched.Submit(Task{ID: "t3", Kind: "test", PixelBytes: 10, Run: func() error { return nil }})
		resultCh <- submitResult{h: h, err: err}
	}()
	time.Sleep(40 * time.Millisecond)
	close(release)

	if err := waitHandle(t, h1); err != nil {
		t.Fatal(err)
	}
	if err := waitHandle(t, h2); err != nil {
		t.Fatal(err)
	}
	var h3 *TaskHandle
	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatal(res.err)
		}
		h3 = res.h
	case <-time.After(2 * time.Second):
		t.Fatal("blocked submit was not admitted after budget release")
	}
	if err := waitHandle(t, h3); err != nil {
		t.Fatal(err)
	}
	sched.Wait()
	snap := sched.Snapshot()
	if snap.WaitCount == 0 || len(snap.Waits) == 0 {
		t.Fatalf("expected recorded queue wait, snapshot=%+v", snap)
	}
	if snap.Waits[0].Reason != "max_queued_tasks" && snap.Waits[0].Reason != "max_queued_pixel_bytes" {
		t.Fatalf("unexpected wait reason %q", snap.Waits[0].Reason)
	}

	_, err = sched.Submit(Task{ID: "too-big", PixelBytes: 2048, Run: func() error { return nil }})
	if code := ErrorCode(err); code != CodeResource {
		t.Fatalf("expected E_RESOURCE for oversized buffer, got %v", err)
	}
}

func TestDecodeInjectionLineFailureRetryIsolation(t *testing.T) {
	shots, err := GenerateSampleSet(FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(filepath.Join(t.TempDir(), "fault.db"))
	cfg.ChunkLineSize = 1
	svc, err := NewService(cfg, DefaultRecipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	svc.InjectTruncatedShot("shot-0001")
	svc.InjectLineFailure("shot-0002", 1, "injected character kernel panic")
	rep, err := svc.Run("fault-injection")
	if err != nil {
		t.Fatal(err)
	}
	s1 := findShotReport(rep, "shot-0001")
	if s1 == nil || s1.Status != "failed" || s1.Error == nil || s1.Error.Code != CodeDecode {
		t.Fatalf("expected E_DECODE shot failure, got %+v", s1)
	}
	l1 := findLineOutcome(rep, "shot-0002", 1)
	if l1 == nil || l1.Status != "failed" || l1.Error == nil {
		t.Fatalf("expected injected line failure, got %+v", l1)
	}
	if l2 := findLineOutcome(rep, "shot-0002", 2); l2 == nil || l2.Status != "success" {
		t.Fatalf("neighbor line polluted: %+v", l2)
	}
	if s3 := findShotReport(rep, "shot-0003"); s3 == nil || s3.Status != "success" {
		t.Fatalf("other shot polluted: %+v", s3)
	}

	svc.ClearTruncatedShot("shot-0001")
	svc.ClearLineFailure("shot-0002", 1)
	retry, err := svc.Run("retry")
	if err != nil {
		t.Fatal(err)
	}
	for _, sr := range retry.Shots {
		if sr.Status != "success" {
			t.Fatalf("shot %s did not recover: %+v", sr.ShotID, sr.Error)
		}
		for _, line := range sr.Lines {
			if line.Status != "success" || line.Recognition == nil {
				t.Fatalf("line %s#%d did not retry successfully: %+v", sr.ShotID, line.LineNo, line.Error)
			}
		}
	}
}

func TestCheckpointRestartRecovery(t *testing.T) {
	shots, _ := GenerateSampleSet(FixedSeed)
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	cfg := DefaultConfig(dbPath)
	svc, err := NewService(cfg, DefaultRecipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := svc.Run("healthy")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	svc2, err := NewService(DefaultConfig(dbPath), DefaultRecipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := svc2.RecoverCurrentCheckpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.DiscardedChunks) != 0 {
		t.Fatalf("healthy restart discarded chunks: %+v", rec)
	}
	if _, err := svc2.Store().db.Exec(`UPDATE checkpoint_lines SET content_hash='tampered-content-hash' WHERE chunk_id=(SELECT chunk_id FROM checkpoint_chunks ORDER BY commit_order LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if err := svc2.Close(); err != nil {
		t.Fatal(err)
	}

	svc3, err := NewService(DefaultConfig(dbPath), DefaultRecipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		t.Fatal(err)
	}
	defer svc3.Close()
	recovery, err := svc3.RecoverCurrentCheckpoints()
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.DiscardedChunks) == 0 || recovery.InvalidChunkID == "" {
		t.Fatalf("tampered checkpoint was not discarded: %+v", recovery)
	}
	recovered, err := svc3.Run("restart-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRunReports(healthy, recovered); err != nil {
		t.Fatalf("recovery mismatch: %v", err)
	}
}

func TestDetectionThresholdEditInvalidatesAndMatchesFull(t *testing.T) {
	shots, _ := GenerateSampleSet(FixedSeed)
	incrementalDB := filepath.Join(t.TempDir(), "threshold-inc.db")
	svc, err := NewService(DefaultConfig(incrementalDB), DefaultRecipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Run("baseline"); err != nil {
		t.Fatal(err)
	}
	items, err := svc.UpdateRecipe(func(r *Recipe) {
		r.Detection.MergeOverlapBPS = 5000
	}, "detection.merge_overlap_bps")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != totalLineCount(shots) {
		t.Fatalf("expected all lines invalidated, got %d", len(items))
	}
	inc, err := svc.Run("threshold-incremental")
	if err != nil {
		t.Fatal(err)
	}
	if inc.CacheMisses != totalLineCount(shots) {
		t.Fatalf("detection edit should recompute all downstream lines, misses=%d", inc.CacheMisses)
	}

	fullDB := filepath.Join(t.TempDir(), "threshold-full.db")
	fullSvc, err := NewService(DefaultConfig(fullDB), svc.Recipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		t.Fatal(err)
	}
	defer fullSvc.Close()
	full, err := fullSvc.Run("threshold-full")
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareRunReports(full, inc); err != nil {
		t.Fatal(err)
	}
}

func TestCompareReportsLocatesEDiff(t *testing.T) {
	shots, _ := GenerateSampleSet(FixedSeed)
	svc, err := NewService(DefaultConfig(filepath.Join(t.TempDir(), "diff.db")), DefaultRecipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	base, err := svc.Run("base")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := svc.Run("cached")
	if err != nil {
		t.Fatal(err)
	}
	changed.Shots[0].Lines[0].Recognition.Text = "CHANGED"
	err = CompareRunReports(base, changed)
	oe, ok := asError(err)
	if !ok || oe.Code != CodeDiff {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	if oe.ShotID == "" || oe.LineNo == 0 || oe.DiffField != "text" {
		t.Fatalf("diff locator incomplete: %+v", oe)
	}
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func waitHandle(t *testing.T, h *TaskHandle) error {
	t.Helper()
	select {
	case err := <-h.Done():
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("task did not finish")
		return nil
	}
}

func findShotReport(r *RunReport, id string) *ShotReport {
	for i := range r.Shots {
		if r.Shots[i].ShotID == id {
			return &r.Shots[i]
		}
	}
	return nil
}

func findLineOutcome(r *RunReport, shotID string, lineNo int) *LineOutcome {
	sr := findShotReport(r, shotID)
	if sr == nil {
		return nil
	}
	for i := range sr.Lines {
		if sr.Lines[i].LineNo == lineNo {
			return &sr.Lines[i]
		}
	}
	return nil
}
