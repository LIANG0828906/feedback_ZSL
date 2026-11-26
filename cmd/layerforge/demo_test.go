package main

import (
	"path/filepath"
	"testing"

	"layerforge/internal/generator"
)

func TestDeterministicDemoAcceptance(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "demo")
	report, err := executeDemo(generator.DefaultSeed, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatal("demo report did not pass")
	}
	if len(report.Schemas) != 3 {
		t.Fatalf("schema checks = %d", len(report.Schemas))
	}
	if report.Restart.HitCount != 8 || report.Incremental.BuildCount != 4 {
		t.Fatalf("restart hits=%d incremental builds=%d", report.Restart.HitCount, report.Incremental.BuildCount)
	}
	if report.PersistedRounds != 2 || report.Resume.BuildCount != 4 {
		t.Fatalf("persisted rounds=%d resumed builds=%d", report.PersistedRounds, report.Resume.BuildCount)
	}
	for _, injection := range report.Injections {
		if !injection.Passed {
			t.Fatalf("injection %s failed: code=%s", injection.Name, injection.Code)
		}
	}
}
