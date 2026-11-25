package diagnostics_test

import (
	"context"
	"path/filepath"
	"testing"

	"semanticshading/internal/diagnostics"
)

func TestAllInjectionChains(t *testing.T) {
	reports := diagnostics.RunAll(context.Background(), filepath.Join(t.TempDir(), "diagnostics"))
	if len(reports) != 8 {
		t.Fatalf("report count = %d", len(reports))
	}
	for _, report := range reports {
		if !report.OK {
			t.Fatalf("injection %s did not produce %s: observed=%s detail=%s error=%#v", report.Kind, report.ExpectedCode, report.ObservedCode, report.Detail, report.Error)
		}
		if report.Error == nil {
			t.Fatalf("injection %s missing structured error", report.Kind)
		}
	}
}
