package strokelab

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestIncrementalStopAndResumeMatchesFull(t *testing.T) {
	suite, err := GenerateSuite(7721)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Samples[2].Project

	fullEngine, err := NewEngine(project)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullEngine.Run(context.Background(), RunOptions{Mode: RunFull})
	if err != nil {
		t.Fatalf("full: %v", err)
	}

	dir := t.TempDir()
	cpPath := filepath.Join(dir, "resume.ckpt")
	cp, err := OpenCheckpoint(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	incEngine, _ := NewEngine(project)
	partial, err := incEngine.Run(context.Background(), RunOptions{
		Mode: RunIncremental, RunID: "test", Checkpoint: cp, StopAfterCommits: 3,
	})
	if !errors.Is(err, ErrSimulatedAbort) {
		t.Fatalf("expected simulated abort, got %v", err)
	}
	if partial == nil || partial.CommittedStrokes != 3 {
		t.Fatalf("partial commits: %+v", partial)
	}

	resumeEngine, _ := NewEngine(project)
	resumed, err := resumeEngine.Run(context.Background(), RunOptions{
		Mode: RunResume, RunID: "test", Checkpoint: cp,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	cmp := CompareEvaluations(full, resumed)
	if !cmp.Equal {
		t.Fatalf("resume differs from full: %+v", cmp.Mismatches)
	}
	if resumed.CommittedStrokes != len(project.Strokes) {
		t.Fatalf("committed %d of %d", resumed.CommittedStrokes, len(project.Strokes))
	}
}
