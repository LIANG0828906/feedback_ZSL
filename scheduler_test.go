package dialogue

import (
	"errors"
	"testing"
	"time"
)

func TestSchedulerCommitsStableOrder(t *testing.T) {
	p, _, _ := sampleProject(t)
	report, err := RunScheduled(p, DefaultSchedulerConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.State.CommittedClips != 4 || report.State.FailedClips != 0 {
		t.Fatalf("unexpected state matrix: %+v", report.State)
	}
	want := []string{"clip-01", "clip-02", "clip-03", "clip-04"}
	if len(report.CommitOrder) != len(want) {
		t.Fatalf("commit order=%v", report.CommitOrder)
	}
	for i := range want {
		if report.CommitOrder[i] != want[i] {
			t.Fatalf("commit order %v want %v", report.CommitOrder, want)
		}
	}
}

func TestSchedulerQueueBudgetBlocksAndRecordsWait(t *testing.T) {
	p, _, _ := sampleProject(t)
	cfg := DefaultSchedulerConfig()
	cfg.MaxParallelism = 1
	cfg.QueueBudget = 1
	release := make(chan struct{})
	started := make(chan string, 1)
	cfg.BeforeJob = func(clipID string) {
		if clipID == "clip-01" {
			started <- clipID
			<-release
		}
	}
	done := make(chan *SchedulerReport, 1)
	go func() {
		report, _ := RunScheduled(p, cfg, nil)
		done <- report
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not start first job")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case report := <-done:
		exhausted := false
		for _, wait := range report.QueueWaits {
			if wait.Reason == "queue_budget_exhausted" && wait.WaitedMicros > 0 {
				exhausted = true
			}
		}
		if !exhausted {
			t.Fatalf("expected recorded blocking queue wait: %+v", report.QueueWaits)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not finish after release")
	}
}

func TestSchedulerBufferBudgetReturnsE_PARA(t *testing.T) {
	p, _, _ := sampleProject(t)
	cfg := DefaultSchedulerConfig()
	cfg.SampleBufferCap = 128
	report, err := RunScheduled(p, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Runs) != 4 {
		t.Fatalf("expected all jobs retained, got %d", len(report.Runs))
	}
	for _, run := range report.Runs {
		if run.ErrorCode != CodePara {
			t.Fatalf("expected E_PARA for %s, got %+v", run.ClipID, run.Error)
		}
	}
}

func TestSchedulerExceptionIsolationCodes(t *testing.T) {
	t.Run("decode", func(t *testing.T) {
		p, _, _ := sampleProject(t)
		bad := CloneProject(p)
		bad.Clips[0].DeclaredSampleCount = len(bad.Clips[0].Samples) + 4096
		report, err := RunScheduled(bad, DefaultSchedulerConfig(), nil)
		if err != nil {
			t.Fatal(err)
		}
		assertOneCode(t, report, CodeDecode, "clip-01")
		if report.State.CommittedClips != 3 {
			t.Fatalf("other clips should commit: %+v", report.State)
		}
	})

	t.Run("stage", func(t *testing.T) {
		p, _, _ := sampleProject(t)
		bad := CloneProject(p)
		bad.Clips[1].ForceStageFailure = true
		report, err := RunScheduled(bad, DefaultSchedulerConfig(), nil)
		if err != nil {
			t.Fatal(err)
		}
		assertOneCode(t, report, CodeStage, "clip-02")
		if report.State.CommittedClips != 3 {
			t.Fatalf("other clips should commit: %+v", report.State)
		}
	})
}

func assertOneCode(t *testing.T, report *SchedulerReport, code, clipID string) {
	t.Helper()
	found := false
	for _, run := range report.Runs {
		if run.ClipID == clipID {
			if run.ErrorCode != code {
				t.Fatalf("%s code=%s err=%+v", clipID, run.ErrorCode, run.Error)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("missing run for %s", clipID)
	}
}

func TestOverlapReturnsE_OVER(t *testing.T) {
	p, _, _ := sampleProject(t)
	p.Clips[1].StartSample = p.Clips[0].StartSample
	report, err := RunScheduled(p, DefaultSchedulerConfig(), nil)
	var be *Error
	if !errors.As(err, &be) || be.Code != CodeOver {
		t.Fatalf("expected E_OVER, got report=%+v err=%#v", report, err)
	}
}
