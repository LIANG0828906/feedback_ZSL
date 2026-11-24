package terrain

import (
	"testing"
)

func TestLocalDeterministicAcceptance(t *testing.T) {
	report, err := RunAcceptance(t.TempDir())
	if err != nil {
		t.Fatalf("RunAcceptance returned runtime error: %v", err)
	}
	if !report.OK {
		for _, check := range report.Checks {
			if !check.Passed {
				t.Errorf("failed check %s: %s", check.Name, check.Detail)
			}
		}
		t.Fatal("acceptance report was not OK")
	}
}
