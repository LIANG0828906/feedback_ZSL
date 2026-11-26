package engine_test

import (
	"context"
	"testing"

	"dialoguefit/engine"
	"dialoguefit/generate"
	"dialoguefit/sched"
	"dialoguefit/state"
)

func TestDialogueFailureIsIsolatedAndRetryable(t *testing.T) {
	project := generate.GenerateProject(generate.DefaultSeed)
	truth := generate.GenerateTruth(project)
	eng := engine.New(project, truth, nil, sched.New(sched.DefaultConfig()))
	eng.InjectedFailures["dlg-02"] = true

	if _, err := eng.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll returned infrastructure error: %v", err)
	}
	if eng.State("dlg-02") != state.Failed {
		t.Fatalf("dlg-02 state=%s want failed", eng.State("dlg-02"))
	}
	if eng.State("dlg-01") != state.Committed || eng.State("dlg-03") != state.Committed {
		t.Fatalf("other dialogues not committed: 01=%s 03=%s", eng.State("dlg-01"), eng.State("dlg-03"))
	}
	if _, ok := eng.Results["dlg-02"]; ok {
		t.Fatal("failed dialogue should not publish a result")
	}
	if _, ok := eng.Results["dlg-01"]; !ok {
		t.Fatal("dlg-01 result missing")
	}

	retryReport, err := eng.RetryDialogue(context.Background(), "dlg-02")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retryReport == nil || retryReport.RecomputedPhonemes != 10 {
		t.Fatalf("retry report=%+v want full dialogue recompute", retryReport)
	}
	if eng.State("dlg-02") != state.Committed {
		t.Fatalf("after retry state=%s want committed", eng.State("dlg-02"))
	}
	if _, ok := eng.Results["dlg-02"]; !ok {
		t.Fatal("retried result missing")
	}
}
