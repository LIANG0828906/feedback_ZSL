package iconmask

import (
	"context"
	"path/filepath"
	"testing"
)

func TestThreePathConsistency(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-128")
	snapshot := NewParameterSnapshot(DefaultRepairConfig())
	cfg := DefaultConsistencyConfig()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "three-path.db")
	cfg.InterruptAfter = 37
	report, err := RunThreePathConsistency(context.Background(), image, snapshot, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !report.MasksEqual || !report.StatsEqual || !report.CommitSequencesEqual {
		t.Fatalf("three paths diverged: %+v", report)
	}
	if report.FirstInconsistentBlock != nil {
		t.Fatalf("unexpected first inconsistent block: %+v", report.FirstInconsistentBlock)
	}
	if report.Incremental.CheckpointCount == 0 || report.Resume.CheckpointCount == 0 {
		t.Fatal("expected persisted stage checkpoints")
	}
	if report.Incremental.CheckpointCount != report.Full.CheckpointCount ||
		report.Resume.CheckpointCount != report.Full.CheckpointCount {
		t.Fatal("stage commit counts must match")
	}
	if report.Incremental.Stats.Count != len(image.Truth.Components) {
		t.Fatalf("expected %d final regions, got %d", len(image.Truth.Components), report.Incremental.Stats.Count)
	}
	if report.Exact == nil || report.Exact.OutsideChanged != 0 {
		t.Fatal("exact path must preserve edge-band outside pixels")
	}
	if report.DualPath != nil && !report.DualPath.Comparison.Passed {
		t.Fatal("embedded dual-path check should pass for default parameters")
	}
}

func TestThreePathEveryCheckpointHasMetadata(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-96")
	snapshot := NewParameterSnapshot(DefaultRepairConfig())
	cfg := DefaultConsistencyConfig()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "metadata.db")
	report, err := RunThreePathConsistency(context.Background(), image, snapshot, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, commit := range report.Incremental.Commits {
		if commit.OutputDigest == "" || commit.CacheKey == "" || commit.ContentHash == "" ||
			commit.ParameterVersion != snapshot.Version || commit.AlgorithmVersion != AlgorithmVersion {
			t.Fatalf("checkpoint metadata is incomplete: %+v", commit)
		}
	}
}

func TestThreePathPropagatesPreviewDivergence(t *testing.T) {
	project := testProject(t)
	image := findImage(t, project, "icon-96")
	snapshot := NewParameterSnapshot(DefaultRepairConfig())
	cfg := DefaultConsistencyConfig()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "divergence.db")
	cfg.Preview.AreaRelativeTolerance = -1
	report, err := RunThreePathConsistency(context.Background(), image, snapshot, cfg)
	domainErr := assertCode(t, err, CodeDivergence)
	if report == nil || report.DualPath == nil || report.DualPath.Comparison.Passed {
		t.Fatal("expected retained divergent dual-path report")
	}
	if domainErr.FirstDiffXY == nil {
		t.Fatal("divergence must carry first differing pixel")
	}
}
