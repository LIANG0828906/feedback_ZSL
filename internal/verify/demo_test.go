package verify_test

import (
	"context"
	"testing"

	"hdrproof/internal/verify"
)

func TestAcceptanceHarness(t *testing.T) {
	report, err := verify.RunAcceptance(context.Background(), verify.Options{
		Root: t.TempDir(), Workers: 4, Buffer: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "pass" || report.Audit == nil || !report.Audit.Passed {
		t.Fatalf("acceptance did not pass: %+v", report)
	}
	if len(report.Comparisons) != 3 {
		t.Fatalf("missing comparisons: %+v", report.Comparisons)
	}
	for _, comparison := range report.Comparisons {
		if !comparison.Equal || comparison.Frames != 95 {
			t.Fatalf("comparison failed: %+v", comparison)
		}
	}
	if len(report.Injections) != 4 || report.BufferBlocks == 0 {
		t.Fatalf("injection or buffer evidence missing: %+v", report)
	}
}
