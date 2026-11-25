package demo

import (
	"context"
	"testing"
	"time"
)

func TestVerifyAllDeterministicLocalSample(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := VerifyAll(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("VerifyAll returned error: %v", err)
	}
	if !report.OK {
		for _, problem := range report.Errors {
			t.Errorf("verification failure: %s", problem)
		}
		t.Fatalf("VerifyAll report is not OK")
	}
	if !report.SchemaCoreEqual || !report.SchemaReportEqual || !report.ConcurrencyEqual {
		t.Fatalf("equality flags: core=%v reports=%v concurrency=%v", report.SchemaCoreEqual, report.SchemaReportEqual, report.ConcurrencyEqual)
	}
	for _, check := range report.MilestoneChecks {
		if !check.OK {
			t.Errorf("milestone %s failed: %s", check.Name, check.Detail)
		}
	}
	for _, check := range report.EditChecks {
		if !check.Comparison.Equal {
			t.Errorf("incremental edit %s mismatch: %v", check.Name, check.Comparison.Differences)
		}
	}
}
