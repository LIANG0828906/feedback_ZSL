package demo_test

import (
	"context"
	"path/filepath"
	"testing"

	"maskpreview/internal/demo"
	"maskpreview/internal/engine"
)

func TestEndToEndDemo(t *testing.T) {
	base := t.TempDir()
	cfg := demo.Config{
		SampleDir:   filepath.Join(base, "samples"),
		ArtifactDir: filepath.Join(base, "artifacts"),
		Seed:        1937,
		Run:         engine.DefaultRunConfig(),
	}
	report, err := demo.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("demo run: %v", err)
	}
	if report.Status != "pass" {
		t.Fatalf("demo status=%s", report.Status)
	}
	for _, sample := range report.Samples {
		if !sample.PixelConsistent || !sample.RegionStatsConsistent || !sample.StageSequenceConsistent {
			t.Fatalf("sample %s inconsistency: %+v", sample.ID, sample)
		}
		if !sample.Preview.WithinBudget {
			t.Fatalf("sample %s preview missed budget", sample.ID)
		}
	}
	for _, name := range []string{"full", "incremental"} {
		if report.Verification[name] == nil || report.Verification[name].Status != "pass" {
			t.Fatalf("verification %s failed", name)
		}
	}
	for _, probe := range report.Exceptions {
		if !probe.OK {
			t.Fatalf("exception probe %s failed: %+v", probe.Name, probe)
		}
	}
	if !report.ResourceBudget.OK {
		t.Fatalf("resource probe failed: %+v", report.ResourceBudget)
	}
	if !report.TamperRecovery.OK {
		t.Fatalf("tamper recovery failed: %+v", report.TamperRecovery)
	}
}
