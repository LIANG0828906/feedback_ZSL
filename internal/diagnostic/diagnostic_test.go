package diagnostic

import (
	"testing"

	"edgerefine/internal/sample"
)

func TestThreePathsRecoveryAndIndependentVerification(t *testing.T) {
	samplesRoot := t.TempDir()
	if _, err := sample.GenerateAll(samplesRoot); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	res, err := RunThreePaths(samplesRoot, "step128", work)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Comparison.AllPixelEqual || !res.Comparison.AccuracyEqual || !res.Comparison.CommitSequencesEqual {
		t.Fatalf("three paths differ: %+v", res.Comparison)
	}
	if !res.Verification.OK || res.Verification.FirstViolation != nil {
		t.Fatalf("verification failed: %+v", res.Verification)
	}
	if res.Verification.Accuracy == nil || !res.Verification.Accuracy.Pass {
		t.Fatalf("independent accuracy failed: %+v", res.Verification.Accuracy)
	}
	if res.Divergence == nil || !res.Divergence.Pass {
		t.Fatalf("preview divergence failed: %+v", res.Divergence)
	}
}
