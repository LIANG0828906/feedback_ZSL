package pipeline_test

import (
	"testing"

	"prosodyalign/internal/pipeline"
)

func TestThreePathsAndTruthAcceptance(t *testing.T) {
	report, err := pipeline.RunThreeWay(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Comparison.AllEqual {
		t.Fatalf("three paths differ: %+v", report.Comparison.FirstMismatch)
	}
	if !report.Comparison.CommitSequencesEqual {
		t.Fatalf("commit sequences differ: %+v", report.Comparison.Mismatches)
	}
	for _, truth := range report.Truth {
		if !truth.EnvelopeExact || !truth.FitExact {
			t.Fatalf("truth mismatch %s: %+v", truth.DialogID, truth)
		}
		if !truth.AccentWithinTol {
			t.Fatalf("accent outside tolerance: %+v", truth)
		}
	}
	if report.RecoveryBoundary == nil || !report.RecoveryBoundary.SnapshotValid {
		t.Fatalf("recovery boundary invalid: %+v", report.RecoveryBoundary)
	}
	if len(report.RecoveryBoundary.CompletedCommitKeys) < 5 {
		t.Fatalf("expected durable prefix including crash fragment, got %+v", report.RecoveryBoundary)
	}
}

func TestIncrementalManifestReusesEnvelopesAndMarksPhonemeEdit(t *testing.T) {
	report, err := pipeline.RunThreeWay(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	weightEpoch := report.Realtime.Epochs[4]
	invalidEnvelopes := 0
	reusedEnvelopes := 0
	for _, item := range weightEpoch.Manifest.Items {
		if item.NodeKind == "phoneme_envelope" {
			invalidEnvelopes++
		}
	}
	for _, item := range weightEpoch.Manifest.ReusedItems {
		if item.NodeKind == "phoneme_envelope" {
			reusedEnvelopes++
		}
	}
	if invalidEnvelopes != 0 {
		t.Fatalf("weight edit must not recompute envelopes, invalid=%d", invalidEnvelopes)
	}
	if reusedEnvelopes == 0 {
		t.Fatal("expected persisted envelope reuse records")
	}
	persistentStretchReuse := 0
	for _, item := range weightEpoch.Manifest.ReusedItems {
		if item.NodeKind == "stretch_coefficient" && item.ReusedFromKey != "" {
			persistentStretchReuse++
		}
	}
	if persistentStretchReuse == 0 {
		t.Fatal("expected unchanged stretch coefficients to be loaded from persistent cache")
	}

	capEpoch := report.Realtime.Epochs[2]
	foundCapReason := false
	for _, item := range capEpoch.Manifest.Items {
		if item.NodeKind == "stretch_coefficient" && item.Reason == "stretch_upper_bound_changed_redistribute_gap" {
			foundCapReason = true
		}
	}
	if !foundCapReason {
		t.Fatalf("missing cap redistribution invalidation: %+v", capEpoch.Manifest.Items)
	}
}
