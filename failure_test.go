package terrain

import (
	"context"
	"testing"
)

func TestFirstBuildFailureThenRetry(t *testing.T) {
	project := GenerateSample(42)
	svc, err := NewService(project, ServiceOptions{DBPath: t.TempDir() + "/first-fail.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	svc.InjectPanicOnce(1)
	manifest, runErr := svc.RebuildAll(context.Background())
	if runErr == nil {
		t.Fatal("expected injected failure")
	}
	if manifest == nil {
		t.Fatal("manifest should still be returned for isolation")
	}
	failed := map[int]bool{}
	for _, stat := range manifest.Blocks {
		if stat.Status == StatusFailed {
			failed[stat.BlockNo] = true
		}
	}
	if len(failed) != 1 || !failed[1] {
		t.Fatalf("expected only block 1 failure, got %+v", manifest.Blocks)
	}

	retryManifest, err := svc.RetryBlock(context.Background(), 1)
	if err != nil {
		t.Fatalf("retry should recover: %v", err)
	}
	for _, stat := range retryManifest.Blocks {
		if stat.BlockNo == 1 && stat.Status != StatusClean {
			t.Fatalf("block 1 status=%s failure=%s", stat.Status, stat.Failure)
		}
	}
	compiled, err := CompileProject(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareMeshMaps(compiled.Meshes, serviceMeshes(svc.State())); err != nil {
		t.Fatal(err)
	}
}
