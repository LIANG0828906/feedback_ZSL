package verify_test

import (
	"context"
	"testing"

	"example.com/multiscreen-cast/samplegen"
	"example.com/multiscreen-cast/verify"
)

func TestThreePathsTruthAndAnomalyChains(t *testing.T) {
	set, err := samplegen.Generate(samplegen.Options{Seed: 303, Screens: 4, Windows: 6, Sessions: 2})
	if err != nil {
		t.Fatal(err)
	}
	report, verifyErr := verify.VerifySample(context.Background(), set, t.TempDir())
	if verifyErr != nil {
		t.Fatal(verifyErr)
	}
	if !report.TruthMatched || !report.ThreePathsMatched {
		t.Fatalf("verification failed: %+v", report)
	}
	if report.RecoveryPointsChecked != 12 {
		t.Fatalf("recovery points=%d want=12", report.RecoveryPointsChecked)
	}
	if report.ForcedStopExitReport {
		t.Fatal("forced stop must not claim an exit report")
	}
	anomalies, anomalyErr := verify.CheckAnomalyChains(context.Background(), set)
	if anomalyErr != nil {
		t.Fatal(anomalyErr)
	}
	if anomalies.Decode != "E_DECODE" || anomalies.Order != "E_ORDER" ||
		anomalies.Cap != "E_CAP" || anomalies.Segment != "E_SEG" {
		t.Fatalf("unexpected anomaly report: %+v", anomalies)
	}
}
