package verify_test

import (
	"context"
	"testing"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/sample"
	"ocrworkbench/internal/verify"
)

func TestVerifierRecomputesMetricsAndLocatesFirstViolation(t *testing.T) {
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.Options{OutputDir: dir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	runner := pipeline.Runner{
		Detector: core.DefaultDetectorParams(),
		Segment:  core.DefaultSegmentParams(),
	}
	run, err := runner.Run(context.Background(), manifest, dir, "full")
	if err != nil {
		t.Fatal(err)
	}
	if issues := verify.VerifyRun(run, manifest); len(issues) > 0 {
		t.Fatalf("clean run failed verification: %s", verify.FormatIssues(issues))
	}

	tampered := run
	tampered.Images = append([]core.ImageResult(nil), run.Images...)
	tampered.Images[0].Lines = append([]core.LineResult(nil), run.Images[0].Lines...)
	tampered.Images[0].Lines[0].Text = "WRONG"
	issues := verify.VerifyRun(tampered, manifest)
	if len(issues) == 0 {
		t.Fatal("tampered run passed verification")
	}
	found := false
	for _, issue := range issues {
		if issue.ImageID == run.Images[0].ImageID && issue.Line == 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("verifier did not locate first violating image/line: %s", verify.FormatIssues(issues))
	}
}

func TestCompareLocatesThreePathDifference(t *testing.T) {
	dir := t.TempDir()
	manifest, err := sample.Generate(sample.Options{OutputDir: dir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	runner := pipeline.Runner{
		Detector: core.DefaultDetectorParams(),
		Segment:  core.DefaultSegmentParams(),
	}
	a, err := runner.Run(context.Background(), manifest, dir, "realtime")
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.RunKind = "full"
	b.Images = append([]core.ImageResult(nil), a.Images...)
	b.Images[0].Lines = append([]core.LineResult(nil), a.Images[0].Lines...)
	b.Images[0].Lines[0].Characters = append([]core.CharResult(nil), a.Images[0].Lines[0].Characters...)
	b.Images[0].Lines[0].Characters[0].Confidence += 0.01
	issues := verify.CompareRuns(a, b)
	if len(issues) == 0 {
		t.Fatal("three-path difference not detected")
	}
	if issues[0].ImageID != a.Images[0].ImageID || issues[0].Line != 0 {
		t.Fatalf("first violation is %+v", issues[0])
	}
}
