package pipeline_test

import (
	"context"
	"path/filepath"
	"testing"

	"semanticshading/internal/pipeline"
	"semanticshading/internal/sample"
)

func TestThreePathsProduceSameResults(t *testing.T) {
	s, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	report, err := pipeline.RunThreePaths(context.Background(), s, pipeline.Options{
		Dir:            filepath.Join(t.TempDir(), "checkpoints"),
		Parallelism:    4,
		QueueBudget:    2,
		ResumeAfter:    3,
		VerifyEachPath: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Comparison.OK {
		for _, diff := range report.Comparison.Differences {
			t.Errorf("difference field=%s region=%d message=%s", diff.Field, diff.RegionID, diff.Message)
		}
		t.Fatal("three paths differ")
	}
	if report.Verification == nil || !report.Verification.OK {
		t.Fatalf("independent verification failed: %#v", report.Verification)
	}
	wantSeq := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	for _, path := range []pipeline.PathResult{report.Realtime, report.Resume, report.Full} {
		if len(path.RunReport.CommitSequence) != len(wantSeq) {
			t.Fatalf("%s sequence = %v", path.Name, path.RunReport.CommitSequence)
		}
		for i := range wantSeq {
			if path.RunReport.CommitSequence[i] != wantSeq[i] {
				t.Fatalf("%s sequence = %v", path.Name, path.RunReport.CommitSequence)
			}
		}
		if path.Verification != nil && !path.Verification.OK {
			t.Fatalf("%s verification failed", path.Name)
		}
	}
}
