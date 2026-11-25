package pipeline

import (
	"context"
	"testing"
)

func TestAcceptanceThreePathsAndTruth(t *testing.T) {
	report, err := RunAcceptance(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("RunAcceptance returned error: %v", err)
	}
	if !report.OK {
		t.Fatalf("acceptance not ok: %s\nfirst diff=%+v\ntruth=%+v", report.Message, report.Diff, report.TruthVerification)
	}
	if report.TruthVerification == nil || !report.TruthVerification.OK {
		t.Fatalf("truth verification failed: %+v", report.TruthVerification)
	}
	for _, id := range []string{"R2", "R3", "R4"} {
		if !contains(report.AbsorptionInvalidation.ReusedRooms, id) {
			t.Fatalf("absorption edit must reuse %s, plan=%+v", id, report.AbsorptionInvalidation)
		}
	}
	if len(report.Preview.ReusedRooms) != 3 || len(report.Preview.RecomputedRooms) != 1 {
		t.Fatalf("boundary preview reuse mismatch: reused=%v recomputed=%v", report.Preview.ReusedRooms, report.Preview.RecomputedRooms)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
