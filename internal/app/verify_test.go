package app_test

import (
	"path/filepath"
	"testing"

	"multiplexgw/internal/app"
)

func TestLocalDeterministicAcceptance(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	artifacts := filepath.Join(root, "artifacts")

	report, err := app.Verify(data, artifacts)
	if err != nil {
		t.Fatalf("Verify returned error: %v\nreport=%+v", err, report)
	}
	if !report.OK {
		t.Fatalf("acceptance report not OK: %+v", report)
	}
	if !report.CrossVersion.Equal {
		t.Fatalf("cross version differences: %v", report.CrossVersion.Diffs)
	}
	if !report.Truth.Match || report.Truth.TraceCount != 42 {
		t.Fatalf("truth report mismatch: %+v", report.Truth)
	}
	if !report.Checkpoint.Match {
		t.Fatalf("checkpoint mismatch: %v", report.Checkpoint.Mismatches)
	}
	for _, ex := range report.Exceptions {
		if !ex.Match {
			t.Fatalf("exception %s expected %s got %s", ex.Case, ex.Expected, ex.ActualCode)
		}
	}
	for _, inc := range report.Incremental.Cases {
		if !inc.SemanticEqual || inc.Failure != "" {
			t.Fatalf("incremental case %s failed: %s", inc.Name, inc.Failure)
		}
	}
}

func TestBudgetParameterRejected(t *testing.T) {
	report, err := app.VerifyWithOptions(t.TempDir(), t.TempDir(), 0, 8)
	if err == nil {
		t.Fatal("expected worker budget error")
	}
	if report != nil && report.OK {
		t.Fatal("report should not be OK for invalid budget")
	}
}
