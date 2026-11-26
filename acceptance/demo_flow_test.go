package acceptance

import (
	"testing"

	"dialoguefit/demo"
)

func TestDemoThreePathsRecheckAndInvalidation(t *testing.T) {
	bundle, err := demo.Run(t.TempDir())
	if err != nil {
		t.Fatalf("demo.Run: %v", err)
	}
	defer bundle.LiveStore.Close()

	report := bundle.Report
	if !report.Scenario.Consistency.OK {
		t.Fatalf("consistency mismatch: %+v", report.Scenario.Consistency)
	}
	if !report.IndependentRecheck.OK {
		t.Fatalf("independent recheck failed: %+v", report.IndependentRecheck.FirstViolation)
	}
	if report.Scenario.Consistency.ComparedFrames != 514 {
		t.Fatalf("frames=%d want 514", report.Scenario.Consistency.ComparedFrames)
	}
	if report.Scenario.Consistency.ComparedStressMarks != 24 {
		t.Fatalf("stress=%d want 24", report.Scenario.Consistency.ComparedStressMarks)
	}
	if report.Scenario.Consistency.ComparedSegments != 60 {
		t.Fatalf("segments=%d want 60", report.Scenario.Consistency.ComparedSegments)
	}

	wantRecomputed := []int{7, 4, 4, 6}
	for i, step := range report.Scenario.EditSteps {
		if step.Report == nil || step.Report.RecomputedPhonemes != wantRecomputed[i] {
			t.Fatalf("step %s recomputed=%v want %d", step.Name, step.Report, wantRecomputed[i])
		}
	}
	if report.Scenario.Resume.Restore.RestoredDialogues != 5 {
		t.Fatalf("restored=%d want 5", report.Scenario.Resume.Restore.RestoredDialogues)
	}
	if len(report.Scenario.FinalCommitOrder) != 60 {
		t.Fatalf("commit order len=%d want 60", len(report.Scenario.FinalCommitOrder))
	}
}
