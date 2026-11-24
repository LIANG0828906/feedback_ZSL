package workbench_test

import (
	"path/filepath"
	"testing"

	"storyboardstudio/internal/dependency"
	"storyboardstudio/internal/model"
	"storyboardstudio/internal/sample"
	"storyboardstudio/internal/workbench"
)

func TestGeneratedSamplesPassAllAcceptanceComparisons(t *testing.T) {
	for _, seed := range sample.DefaultSeeds() {
		project := sample.Generate(seed)
		report, err := workbench.VerifyProject(project, t.TempDir())
		if err != nil {
			t.Fatalf("seed %d verification error: %v", seed, err)
		}
		if !report.OK {
			t.Fatalf("seed %d verification failed: %+v", seed, report)
		}
		if report.ShotCount == 0 {
			t.Fatalf("seed %d produced no shots", seed)
		}
	}
}

func TestDurationEditInvalidatesSuffixAndReusesPersistentCache(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	wb, err := workbench.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	project := sample.Generate(sample.DefaultSeeds()[1])
	if err := wb.AddProject(project); err != nil {
		t.Fatal(err)
	}
	baseline, err := wb.RunCheckpointed(project.ID, "baseline", -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := wb.Close(); err != nil {
		t.Fatal(err)
	}

	wb, err = workbench.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Close()
	if err := wb.AddProject(project); err != nil {
		t.Fatal(err)
	}
	workbench.SetLastResultForTest(wb, project.ID, baseline)

	lastNode := project.Truth.EventOrder[len(project.Truth.EventOrder)-1]
	node := findNode(project, lastNode)
	newDuration := node.Shots[0].DurationMS + 250
	report, err := wb.EditAndReplay(project.ID, "edit-session", workbench.EditSpec{
		NodeID: lastNode, Kind: dependency.EditDuration, LocalIndex: 0, DurationMS: &newDuration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Comparison == nil || !report.Comparison.OK {
		t.Fatalf("incremental replay mismatch: %+v", report.Comparison)
	}
	if len(report.CacheMisses) != 1 || report.CacheMisses[0] != len(baseline.Shots)-1 {
		t.Fatalf("expected only final shot miss, got hits=%v misses=%v", report.CacheHits, report.CacheMisses)
	}
	if len(report.CacheHits) != len(baseline.Shots)-1 {
		t.Fatalf("expected all previous shots cache hits, got %d", len(report.CacheHits))
	}
	foundManifestShot := false
	for _, invalidation := range report.Plan.Invalidations {
		if invalidation.Kind == "shot" && invalidation.ShotIndex == len(baseline.Shots)-1 &&
			invalidation.NodeID == lastNode && invalidation.Reason != "" {
			foundManifestShot = true
		}
	}
	if !foundManifestShot {
		t.Fatalf("manifest missing final shot invalidation: %+v", report.Plan.Invalidations)
	}
}

func TestPreconditionEditPropagatesToCausalAndMutexDownstream(t *testing.T) {
	wb, err := workbench.New(filepath.Join(t.TempDir(), "precondition.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Close()
	project := sample.Generate(sample.DefaultSeeds()[1])
	if err := wb.AddProject(project); err != nil {
		t.Fatal(err)
	}
	if _, err := wb.RunContinuous(project.ID); err != nil {
		t.Fatal(err)
	}

	branchNodeID := project.Truth.EventOrder[2]
	condition := findNode(project, branchNodeID).Preconditions
	condition.All = append(condition.All, "world_established")
	report, err := wb.EditAndReplay(project.ID, "precondition-session", workbench.EditSpec{
		NodeID: branchNodeID, Kind: dependency.EditPreconditions, Condition: &condition,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Comparison == nil || !report.Comparison.OK {
		t.Fatalf("comparison failed: %+v", report.Comparison)
	}
	for _, required := range []string{branchNodeID, "n04_join", "n05_final_choice", "n06_epilogue"} {
		if !contains(report.ReplayOrder, required) {
			t.Fatalf("replay order %v does not contain %s", report.ReplayOrder, required)
		}
	}
	for _, clean := range []string{"n00_world", "n01_office"} {
		if contains(report.ReplayOrder, clean) {
			t.Fatalf("unrelated upstream node %s was invalidated: %v", clean, report.ReplayOrder)
		}
	}
	if len(report.CacheHits) == 0 {
		t.Fatal("expected clean upstream shots to be reused from cache")
	}
}

func findNode(project *model.Project, id string) *model.Node {
	for i := range project.Nodes {
		if project.Nodes[i].ID == id {
			return &project.Nodes[i]
		}
	}
	return nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
