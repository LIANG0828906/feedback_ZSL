package downsample

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRunDemoProducesPassingReport(t *testing.T) {
	report, err := RunDemo(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatalf("RunDemo: %v", err)
	}
	if !report.AllPassed {
		t.Fatalf("demo checks failed: %v", report.Checks)
	}
	if !report.SourceUnchanged {
		t.Fatalf("injections modified source schema bytes")
	}
	if len(report.Injections) != len(InjectionCases()) {
		t.Fatalf("injection count = %d, want %d", len(report.Injections), len(InjectionCases()))
	}
}

func TestStreamInterruptLeavesLastCompleteCommit(t *testing.T) {
	bundle := testBundle(t)
	project, err := LoadProject(bundle.Documents[SchemaV1])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signal.sqlite")
	session, coded := NewSession(project, DefaultBudget(), path)
	if coded != nil {
		t.Fatal(coded)
	}
	committed, coded := session.AppendAndCheckpoint(context.Background(), Point{
		MetricID: "cpu.usage", TimestampMS: bundle.BaseTimeMS + 8*60_000,
		Value: 41, Weight: 1000,
	})
	if coded != nil || committed == nil {
		t.Fatalf("commit first point: report=%v err=%v", committed, coded)
	}
	committedSeq := session.Engine.GlobalSequence()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, interrupted := session.AppendAndCheckpoint(ctx,
		Point{MetricID: "cpu.usage", TimestampMS: bundle.BaseTimeMS + 9*60_000, Value: 51, Weight: 1000},
		Point{MetricID: "cpu.usage", TimestampMS: bundle.BaseTimeMS + 10*60_000, Value: 52, Weight: 1000},
	)
	if !IsCode(interrupted, CodeStreamInterrupted) {
		t.Fatalf("expected %s, got %v", CodeStreamInterrupted, interrupted)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	restartProject, err := LoadProject(bundle.Documents[SchemaV1])
	if err != nil {
		t.Fatal(err)
	}
	recovered, recovery, coded := RecoverOrInitialize(restartProject, DefaultBudget(), path)
	if coded != nil {
		t.Fatal(coded)
	}
	defer recovered.Close()
	if recovery.GlobalSeq != committedSeq {
		t.Fatalf("recovery seq = %d, want last committed %d", recovery.GlobalSeq, committedSeq)
	}
	w8, qerr := recovered.Engine.QueryWindow("cpu.usage", bundle.BaseTimeMS+8*60_000)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if w8.PointCount != 1 {
		t.Fatalf("committed point count = %d, want 1", w8.PointCount)
	}
	w9, qerr := recovered.Engine.QueryWindow("cpu.usage", bundle.BaseTimeMS+9*60_000)
	if qerr != nil {
		t.Fatal(qerr)
	}
	if w9.PointCount != 0 {
		t.Fatalf("interrupted/uncommitted point became visible: count=%d", w9.PointCount)
	}
}
