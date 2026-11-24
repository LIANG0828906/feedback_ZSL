package acceptance

import (
	"encoding/json"
	"testing"
)

func TestLocalDeterministicSampleAcceptance(t *testing.T) {
	report, err := Run()
	if err != nil {
		data, _ := json.MarshalIndent(report, "", "  ")
		t.Fatalf("acceptance failed: %v\nreport=%s", err, string(data))
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("\n%s", string(data))

	if len(report.ThreeListenerL1Paths) != 3 {
		t.Fatalf("expected three S1/S2/S3 -> L1 paths, got %v", report.ThreeListenerL1Paths)
	}
	if !report.IncrementalCompareOK || !report.RecoveryCompareOK {
		t.Fatalf("three-way consistency flags not set: %+v", report)
	}
	if report.ScopeErrorCode != "E_SCOPE" || report.ScopeFirstRoom == "" {
		t.Fatalf("expected E_SCOPE with first false invalidation room, got %+v", report)
	}
	if !report.SchedulerWaitRecorded {
		t.Fatal("expected bounded scheduler to record blocking wait")
	}
	for _, check := range report.PathChecks {
		if !check.ChainOK || !check.WallClear {
			t.Fatalf("path geometry mismatch: %+v", check)
		}
	}
}
