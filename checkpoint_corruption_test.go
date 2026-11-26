package strokelab

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestCorruptMiddleCheckpointRecordIsRejected(t *testing.T) {
	suite, err := GenerateSuite(4545)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Samples[2].Project
	path := t.TempDir() + "/corrupt.ckpt"
	cp, err := OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := NewEngine(project)
	_, err = engine.Run(context.Background(), RunOptions{
		Mode: RunIncremental, RunID: "corrupt", Checkpoint: cp, StopAfterCommits: 4,
	})
	if err != ErrSimulatedAbort {
		t.Fatalf("stop: %v", err)
	}
	_ = cp.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) < 4 {
		t.Fatalf("unexpected checkpoint lines: %d", len(lines))
	}
	// Corrupt a middle record while leaving its newline in place.
	line := lines[2]
	if len(line) < 10 {
		t.Fatal("record too short")
	}
	line[5] ^= 0x01
	corrupt := bytes.Join(lines, nil)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCheckpoint(path); ErrorCode(err) != CodeCheckpoint {
		t.Fatalf("expected E_CKPT for corrupt middle record, got %v", err)
	}
}
