package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIGenerateValidateAndVerify(t *testing.T) {
	root := t.TempDir()
	samples := filepath.Join(root, "samples")
	out := filepath.Join(root, "verification")
	if err := run([]string{"generate", "-out", samples}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"validate", "-samples", samples}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-samples", samples, "-out", out, "-checkpoints", filepath.Join(out, "checkpoints")}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "verification.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report VerificationReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "pass" || len(report.Glyphs) != 3 {
		t.Fatalf("unexpected report: status=%s glyphs=%d", report.Status, len(report.Glyphs))
	}
	for _, check := range report.Checks {
		if !check.Passed {
			t.Fatalf("failed check: %+v", check)
		}
	}
	for _, name := range []string{"outline.png", "bands.png", "mask.png", "density.png", "summary.json"} {
		if _, err := os.Stat(filepath.Join(out, report.Glyphs[0].GlyphID, name)); err != nil {
			t.Fatalf("missing artifact %s: %v", name, err)
		}
	}
}
