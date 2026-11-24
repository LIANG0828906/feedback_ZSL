package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"audioforge/internal/app"
	"audioforge/internal/config"
	"audioforge/internal/errs"
)

func TestStartupProducesCompleteDiagnosticReport(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DevicePort = 0
	cfg.DBPath = filepath.Join(dir, "checkpoints.db")
	cfg.SampleDir = filepath.Join(dir, "samples")
	cfg.Workers = 2
	cfg.BufferPerWorker = 2
	cfg.DeviceTimeoutMS = 100
	runner, err := app.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	report := runner.Report()
	if report == nil {
		t.Fatal("nil report")
	}
	if report.Truth.Verification != "generator truth matched" {
		t.Fatalf("truth not verified: %+v", report.Truth)
	}
	if len(report.PathChecks) != 3 {
		t.Fatalf("path checks = %d", len(report.PathChecks))
	}
	if report.Checkpoint.Windows == 0 || report.Checkpoint.Packets == 0 || !report.Checkpoint.Complete {
		t.Fatalf("invalid checkpoint report: %+v", report.Checkpoint)
	}
	if len(report.CleanAlignment.Issues) != 0 {
		t.Fatalf("clean alignment issues: %+v", report.CleanAlignment.Issues)
	}
	codes := map[errs.Code]bool{}
	for _, issue := range report.FaultAlignment.Issues {
		codes[issue.Code] = true
	}
	for _, code := range []errs.Code{errs.EMiss, errs.EDup, errs.ETimeout} {
		if !codes[code] {
			t.Fatalf("missing injected anomaly %s: %+v", code, report.FaultAlignment.Issues)
		}
	}
	if report.ForcedStopCode != string(errs.ETerminated) {
		t.Fatalf("forced stop code = %q", report.ForcedStopCode)
	}
	if len(report.Branches) != 3 || len(report.Channels) != 3 {
		t.Fatalf("unexpected branch/channel count: %d/%d", len(report.Branches), len(report.Channels))
	}
	if len(report.Invalidations) < 4 {
		t.Fatalf("expected invalidation examples, got %d", len(report.Invalidations))
	}
}
