package app

import (
	"context"
	"testing"
	"time"
)

func TestFullLocalVerificationPasses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := RunVerification(ctx, Options{
		OutDir:          t.TempDir(),
		Seed:            20261125,
		SampleCount:     2,
		WindowSize:      2,
		Parallel:        2,
		BufferPerWorker: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		for _, check := range report.Checks {
			if !check.OK {
				t.Errorf("check %s failed: %s", check.Name, check.Detail)
			}
		}
		for _, project := range report.Projects {
			t.Errorf("project %+v", project)
		}
		t.Fatal("verification report was not OK")
	}
}
