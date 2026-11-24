package service

import (
	"testing"

	"strata/internal/model"
)

func TestPathDifferenceReturnsEDiffWithFirstChunk(t *testing.T) {
	first, same := firstSequenceDifference(
		[]string{"C00", "C01", "C02", "C03"},
		[]string{"C00", "C01", "C100", "C101"},
	)
	if same || first != "C02!=C100" {
		t.Fatalf("unexpected sequence diff %q same=%v", first, same)
	}
	err := pathDiffError("verify_paths.sequence", first, nil)
	if err.Code != model.EDIFF {
		t.Fatalf("expected E_DIFF, got %s", err.Code)
	}
	if err.Details["firstDifferingChunk"] != first {
		t.Fatalf("first differing chunk missing: %+v", err.Details)
	}
}
