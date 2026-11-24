package acceptance

import (
	"context"
	"path/filepath"
	"testing"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/generator"
)

func TestFullAcceptance(t *testing.T) {
	dir := t.TempDir()
	report, err := Run(context.Background(), Options{
		SampleDir: filepath.Join(dir, "samples"),
		DBPath:    filepath.Join(dir, "checkpoints.db"),
		Seed:      generator.DefaultSeed,
		Params:    domain.DefaultParameters(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || !report.ThreePaths.Equal || !report.Recovery.OK || !report.Invalidation.OK {
		t.Fatalf("acceptance failed: %+v", report)
	}
	for _, project := range report.Projects {
		if !project.Monotonic || project.FailedPoints != 0 || project.SourceMismatches != 0 {
			t.Fatalf("project failed: %+v", project)
		}
	}
	if len(report.Faults) != 4 {
		t.Fatalf("expected four fault chains, got %d", len(report.Faults))
	}
}
