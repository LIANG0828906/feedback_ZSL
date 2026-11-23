package verify_test

import (
	"path/filepath"
	"testing"

	"mocapworkbench/internal/loader"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/sample"
	"mocapworkbench/internal/verify"
)

func TestThreePathsAndScaleAudit(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 4208691); err != nil {
		t.Fatal(err)
	}
	medium, err := loader.LoadClip(filepath.Join(dir, "clips", "clip_medium.json"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := loader.LoadTarget(filepath.Join(dir, "targets", "target_tall_29.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Run(medium, target, model.DefaultConfig(), filepath.Join(t.TempDir(), "verify.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllEqual {
		t.Fatalf("three paths differ: %#v", report.Comparisons)
	}
	if len(report.Comparisons) != 3 {
		t.Fatalf("expected 3 comparisons, got %d", len(report.Comparisons))
	}
	if report.ScaleAudit == nil || !report.ScaleAudit.Performed || !report.ScaleAudit.WithinTolerance {
		t.Fatalf("expected passing scale audit, got %#v", report.ScaleAudit)
	}
	if report.ResumeAction != "resumed" {
		t.Fatalf("expected resumed path, got %s", report.ResumeAction)
	}
	for _, comparison := range report.Comparisons {
		if comparison.FirstDiff != nil {
			t.Fatalf("%s has unexpected diff: %#v", comparison.Name, comparison.FirstDiff)
		}
		for _, value := range comparison.Heatmap {
			if value != 0 {
				t.Fatalf("%s heatmap should be zero, got %f", comparison.Name, value)
			}
		}
	}
}
