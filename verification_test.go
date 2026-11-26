package silhouette

import (
	"context"
	"testing"
)

func TestVerificationReportPasses(t *testing.T) {
	report, err := Verify(context.Background(), DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllPassed {
		t.Fatalf("verification failed:\n%s", CompactVerification(report))
	}
	if len(report.VersionLoads) != 3 || len(report.RoundTrips) != 2 {
		t.Fatalf("incomplete verification report: %+v", report)
	}
}
