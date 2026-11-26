package acceptance_test

import (
	"testing"

	"volumekernel/internal/acceptance"
	"volumekernel/internal/generator"
)

func TestAcceptanceEndToEnd(t *testing.T) {
	dir := t.TempDir()
	report, err := acceptance.Run(acceptance.Options{
		Seed: generator.DefaultSeed, CameraIndex: 2, OutDir: dir, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.ThreePathsEqual {
		t.Fatal("three exact paths differ")
	}
	if !report.PreviewDeadlineMet || report.PreviewCompare.OverToleranceRate != 0 {
		t.Fatalf("preview gate failed: %+v", report.PreviewCompare)
	}
	if report.Invalidation.AffectedBlocks == 0 ||
		report.Invalidation.CacheHits != report.Invalidation.UnaffectedBlocks ||
		report.Invalidation.CacheMisses != report.Invalidation.AffectedBlocks {
		t.Fatalf("invalidation report failed: %+v", report.Invalidation)
	}
	if len(report.Exceptions) != 7 {
		t.Fatalf("expected 7 exception chains, got %d", len(report.Exceptions))
	}
	if len(report.Convergence) != 5 {
		t.Fatalf("expected 5 convergence points, got %d", len(report.Convergence))
	}
}
