package strokelab

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointTailTruncationAndVersionMismatch(t *testing.T) {
	suite, err := GenerateSuite(1313)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Samples[2].Project
	fullEngine, _ := NewEngine(project)
	full, err := fullEngine.Run(context.Background(), RunOptions{Mode: RunFull})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tail.ckpt")
	cp, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := NewEngine(project)
	_, err = engine.Run(context.Background(), RunOptions{
		Mode: RunIncremental, RunID: "tail", Checkpoint: cp, StopAfterCommits: 4,
	})
	if err != ErrSimulatedAbort {
		t.Fatalf("stop err=%v", err)
	}
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("partial-record-without-newline"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	reopened, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	status := reopened.Status()
	if !status.TailIgnored || status.Records != 4 {
		t.Fatalf("tail recovery status: %+v", status)
	}
	resumeEngine, _ := NewEngine(project)
	resumed, err := resumeEngine.Run(context.Background(), RunOptions{
		Mode: RunResume, RunID: "tail", Checkpoint: reopened,
	})
	if err != nil {
		t.Fatalf("resume after tail: %v", err)
	}
	if cmp := CompareEvaluations(full, resumed); !cmp.Equal {
		t.Fatalf("tail resume mismatch: %+v", cmp.Mismatches)
	}
	_ = reopened.Close()

	// A structurally valid checkpoint with a different project hash must not
	// be resumed.
	changed := project.Clone()
	changed.Strokes[0].Weight--
	mismatchEngine, _ := NewEngine(changed)
	cp2, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cp2.Close()
	_, err = mismatchEngine.Run(context.Background(), RunOptions{
		Mode: RunResume, RunID: "tail", Checkpoint: cp2,
	})
	if ErrorCode(err) != CodeCheckpoint {
		t.Fatalf("expected E_CKPT version mismatch, got %v", err)
	}
}
