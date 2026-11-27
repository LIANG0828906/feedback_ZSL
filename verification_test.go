package terrainbake

import (
	"context"
	"testing"
)

func TestThreePathsAreItemByItemEqual(t *testing.T) {
	p := testProject(t)
	cfg := EngineConfig{MaxParallelism: 3, MaxVerticesPerThread: 4096, MeshMemoryBudget: 2 << 20}
	report, err := VerifyThreePaths(context.Background(), p, cfg, DefaultVerificationEdits(), t.TempDir())
	if err != nil {
		t.Fatalf("three-path verification: %v", err)
	}
	if !report.Equal || len(report.Paths) != 3 {
		t.Fatalf("report = %+v", report)
	}
	fp := report.Paths[0].Fingerprint
	for _, run := range report.Paths {
		if run.Fingerprint == "" || run.Fingerprint != fp {
			t.Fatalf("%s fingerprint %s want %s", run.Name, run.Fingerprint, fp)
		}
		if len(run.CommitSequence) != len(p.Blocks) {
			t.Fatalf("%s commit sequence = %v", run.Name, run.CommitSequence)
		}
	}
	if report.Paths[0].ReusedCount == 0 || report.Paths[1].ReusedCount == 0 {
		t.Fatalf("incremental/resume did not reuse: %d/%d", report.Paths[0].ReusedCount, report.Paths[1].ReusedCount)
	}
	if report.Paths[2].WrittenCount != len(p.Blocks) {
		t.Fatalf("full rebuild wrote %d blocks", report.Paths[2].WrittenCount)
	}
	for _, cmp := range report.Comparisons {
		if !cmp.Equal {
			t.Fatalf("comparison failed: %+v", cmp)
		}
	}
}
