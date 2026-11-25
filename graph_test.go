package macrokit

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDependencyGraphPreciseConditionInvalidation(t *testing.T) {
	sample := findSample(t, GenerateSamples(FixedSampleSeed), "dependency-chain")
	cfg := ExecutorConfig{FrameCount: sample.Truth.FrameCount}
	runner, err := NewIncrementalRunner(sample.Project, sample.Events, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.RunEmpty(); err != nil {
		t.Fatal(err)
	}

	newGraph, report, err := runner.Graph.EditCondition("d_a", keyPressedCondition("KeyC"))
	if err != nil {
		t.Fatal(err)
	}
	runner.Graph = newGraph
	wantBranches := []string{"d_a", "d_b", "d_c"}
	if !reflect.DeepEqual(report.BranchIDs, wantBranches) {
		t.Fatalf("condition invalidation branches=%v want %v", report.BranchIDs, wantBranches)
	}
	for _, item := range report.Items {
		if item.BranchID == "d_ind" {
			t.Fatalf("unaffected d_ind invalidated: %+v", item)
		}
		if item.Reason == "" {
			t.Fatal("invalidation item missing reason")
		}
	}
	if err := runner.Invalidate(report); err != nil {
		t.Fatal(err)
	}

	incremental, recompute, err := runner.Recompute()
	if err != nil {
		t.Fatal(err)
	}
	empty, err := RunProject(sample.Project, sample.Events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(CommitIDs(empty.Commits), recompute.CommitSequence) {
		t.Fatalf("commit sequence empty=%v incremental=%v", CommitIDs(empty.Commits), recompute.CommitSequence)
	}
	if err := CompareResults(empty, incremental); err != nil {
		t.Fatalf("incremental replay mismatch: %v", err)
	}
	assertStringList(t, recompute.ReusedBranches, []string{"d_ind"})
	assertStringList(t, recompute.RecomputedBranches, []string{"d_a", "d_b", "d_c"})
}

func TestDependencyGraphPreciseDelayInvalidation(t *testing.T) {
	sample := findSample(t, GenerateSamples(FixedSampleSeed+7), "dependency-chain")
	cfg := ExecutorConfig{FrameCount: 15}
	runner, err := NewIncrementalRunner(sample.Project, sample.Events, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.RunEmpty(); err != nil {
		t.Fatal(err)
	}

	report, err := runner.Graph.EditBranchDelay("d_b", 3)
	if err != nil {
		t.Fatal(err)
	}
	wantBranches := []string{"d_b", "d_c"}
	if !reflect.DeepEqual(report.BranchIDs, wantBranches) {
		t.Fatalf("delay invalidation branches=%v want %v", report.BranchIDs, wantBranches)
	}
	if err := runner.Invalidate(report); err != nil {
		t.Fatal(err)
	}
	incremental, recompute, err := runner.Recompute()
	if err != nil {
		t.Fatal(err)
	}
	empty, err := RunProject(sample.Project, sample.Events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(CommitIDs(empty.Commits), recompute.CommitSequence) {
		t.Fatal("incremental and empty-cache commit sequences differ")
	}
	if err := CompareResults(empty, incremental); err != nil {
		t.Fatal(err)
	}
	assertStringList(t, recompute.ReusedBranches, []string{"d_a", "d_ind"})
	assertStringList(t, recompute.RecomputedBranches, []string{"d_b", "d_c"})
}

func TestPersistentBranchCacheAcrossRestart(t *testing.T) {
	ctx := context.Background()
	sample := findSample(t, GenerateSamples(FixedSampleSeed+9), "dependency-chain")
	cfg := ExecutorConfig{FrameCount: sample.Truth.FrameCount}
	dbPath := filepath.Join(t.TempDir(), "cache.sqlite")

	store, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewIncrementalRunner(sample.Project, sample.Events, cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, seq, err := runner.RunEmpty(); err != nil {
		t.Fatal(err)
	} else if len(seq) != 4 {
		t.Fatalf("commit sequence len=%d", len(seq))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := NewIncrementalRunner(sample.Project, sample.Events, cfg, reopened)
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.Cache.BranchIDs(); len(got) != 4 {
		t.Fatalf("persistent cache branch ids=%v", got)
	}

	report, err := restarted.Graph.EditBranchDelay("d_b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Invalidate(report); err != nil {
		t.Fatal(err)
	}
	entries, err := reopened.LoadBranchCache(sample.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, entry := range entries {
		ids[entry.BranchID] = true
	}
	if ids["d_b"] || ids["d_c"] {
		t.Fatalf("invalidated persistent entries remain: %v", ids)
	}
	if !ids["d_a"] || !ids["d_ind"] {
		t.Fatalf("unaffected persistent entries missing: %v", ids)
	}
	_ = ctx
}

func assertStringList(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
}
