package acceptance

import (
	"testing"
)

func TestLocalDeterministicAcceptance(t *testing.T) {
	report, err := Run(Options{WorkDir: t.TempDir(), MaxParallelism: 4})
	if err != nil {
		t.Fatalf("acceptance run failed: %v", err)
	}
	if !report.OK {
		for _, check := range report.Cases {
			if !check.Pass {
				t.Errorf("check %s failed [%s]: %s %v", check.Name, check.Code, check.Detail, check.Mismatch)
			}
		}
		t.Fatalf("deterministic acceptance report did not pass")
	}
	if report.Precision == nil || !report.Precision.Pass {
		t.Fatalf("precision verification failed: %+v", report.Precision)
	}
}
