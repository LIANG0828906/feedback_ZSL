package verify

import (
	"context"
	"testing"
)

func TestRunAllLocalAcceptance(t *testing.T) {
	report := RunAll(context.Background(), t.TempDir())
	failed := 0
	for _, check := range report.Checks {
		if !check.OK {
			failed++
			t.Errorf("%s/%s: %s", check.Category, check.Name, check.Detail)
		}
	}
	if failed > 0 {
		t.Fatalf("%d acceptance checks failed", failed)
	}
}
