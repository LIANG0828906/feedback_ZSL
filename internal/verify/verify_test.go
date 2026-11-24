package verify

import (
	"testing"

	"terrainnormalverify/internal/engine"
	"terrainnormalverify/internal/sample"
)

func TestEndToEndVerification(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	report, err := Run(project, t.TempDir(), engine.DefaultBudget())
	if err != nil {
		t.Fatalf("verification failed: %v", err)
	}
	if !report.Passed {
		for _, check := range report.Checks {
			if !check.Passed {
				t.Errorf("failed check %s: %s", check.Name, check.Detail)
			}
		}
		t.Fatal("verification report did not pass")
	}
	for _, check := range report.Checks {
		if !check.Passed {
			t.Errorf("failed check %s: %s", check.Name, check.Detail)
		}
	}
}
