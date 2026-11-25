package plancache

import (
	"testing"
)

func TestBudgetValidationCodes(t *testing.T) {
	if _, err := NewRuntimeGuard(0, 4); err == nil {
		t.Fatal("expected worker budget error")
	} else {
		assertCode(t, err, CodeBudgetWorkersInvalid)
	}
	if _, err := NewRuntimeGuard(4, 0); err == nil {
		t.Fatal("expected buffer budget error")
	} else {
		assertCode(t, err, CodeBudgetBufferInvalid)
	}

	p := GenerateProject(DefaultSeed)
	p.Runtime.Workers = -2
	_, _, err := RunParallelPipeline(p)
	assertCode(t, err, CodeBudgetWorkersInvalid)

	p = GenerateProject(DefaultSeed)
	p.Runtime.SessionBuffer = -1
	_, _, err = RunParallelPipeline(p)
	assertCode(t, err, CodeBudgetBufferInvalid)
}

func TestDeterministicBackpressureCodes(t *testing.T) {
	guard, err := NewRuntimeGuard(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	releaseSession, err := guard.TryReserveSession("session-full")
	if err != nil {
		t.Fatal(err)
	}
	_, err = guard.TryReserveSession("session-full")
	assertCode(t, err, CodeBackpressureBuffer)
	releaseSession()

	releaseWorker, err := guard.TryAcquireWorker()
	if err != nil {
		t.Fatal(err)
	}
	_, err = guard.TryAcquireWorker()
	assertCode(t, err, CodeBackpressureWorkers)
	releaseWorker()

	ran := false
	err = ExecuteBounded(guard, "session-ok", func() error { ran = true; return nil })
	if err != nil || !ran {
		t.Fatalf("ExecuteBounded should run after tokens released: ran=%v err=%v", ran, err)
	}
}

func TestSessionMetricsRecorded(t *testing.T) {
	p := GenerateProject(DefaultSeed)
	report, _, err := RunParallelPipeline(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Metrics) < 4 {
		t.Fatalf("expected metrics for four sessions, got %+v", report.Metrics)
	}
	counts := map[string]int{}
	for _, m := range report.Metrics {
		if m.RequestCount != len(m.ObservationsMicros) {
			t.Fatalf("observation count mismatch: %+v", m)
		}
		counts[m.SessionID] = m.RequestCount
	}
	if counts["session-A"] != 5 || counts["session-C"] != 2 {
		t.Fatalf("unexpected per-session counts: %+v", counts)
	}
}
