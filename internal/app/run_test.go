package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"inputkernel/internal/app"
	"inputkernel/internal/samplegen"
)

func TestExecuteWritesSuccessfulReproducibleReport(t *testing.T) {
	root := t.TempDir()
	samples := filepath.Join(root, "samples")
	store := filepath.Join(root, "snapshots")
	reportPath := filepath.Join(root, "artifacts", "report.json")
	if _, err := samplegen.Write(samples, samplegen.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	report, err := app.Execute(context.Background(), app.Options{
		SamplesDir: samples, StoreDir: store, ReportPath: reportPath,
		Workers: 3, Buffer: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" {
		t.Fatalf("unexpected status %s", report.Status)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range report.Scenarios {
		if !scenario.Truth.Pass || !scenario.Incremental.Comparison.Equal ||
			!scenario.Snapshot.RestoreEqual || !scenario.Snapshot.RecoveryEqual {
			t.Fatalf("scenario %s failed: %+v", scenario.ID, scenario)
		}
		if scenario.Snapshot.StaleCode != "E_VER" {
			t.Fatalf("scenario %s did not report stale E_VER", scenario.ID)
		}
	}
}
