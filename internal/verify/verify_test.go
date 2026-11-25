package verify_test

import (
	"testing"

	"credrot/internal/verify"
)

func TestDeterministicAcceptanceSuite(t *testing.T) {
	for _, seed := range []int64{1, 42, 20261126} {
		report, err := verify.Run(seed, "")
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for _, check := range report.Checks {
			if !check.OK {
				t.Fatalf("seed %d: check %s failed: %s", seed, check.Name, check.Details)
			}
		}
		if !report.CheckpointOK || !report.AnomaliesOK || !report.CacheReuse {
			t.Fatalf("seed %d: incomplete acceptance report: %+v", seed, report)
		}
	}
}
