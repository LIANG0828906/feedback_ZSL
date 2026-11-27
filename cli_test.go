package grid

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type summaryHashes struct {
	Tiles []struct {
		Col         int    `json:"col"`
		Row         int    `json:"row"`
		ContentHash string `json:"contentHash"`
	} `json:"tiles"`
}

func buildCLI(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "ballisticgrid")
	build := exec.Command("go", "build", "-o", exe, "./cmd/ballisticgrid")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build cli: %v\n%s", err, out)
	}
	return exe
}

func TestCrossProcessVerifyHashesStable(t *testing.T) {
	exe := buildCLI(t)
	dir1 := filepath.Join(t.TempDir(), "run1")
	dir2 := filepath.Join(t.TempDir(), "run2")
	for _, dir := range []string{dir1, dir2} {
		cmd := exec.Command(exe, "verify", "--dir", dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("verify %s: %v\n%s", dir, err, out)
		}
	}
	var s1, s2 summaryHashes
	readSummary(t, filepath.Join(dir1, "summary.json"), &s1)
	readSummary(t, filepath.Join(dir2, "summary.json"), &s2)
	if len(s1.Tiles) == 0 || len(s1.Tiles) != len(s2.Tiles) {
		t.Fatalf("tile summaries %d/%d", len(s1.Tiles), len(s2.Tiles))
	}
	for i := range s1.Tiles {
		if s1.Tiles[i].ContentHash != s2.Tiles[i].ContentHash {
			t.Fatalf("tile %d hash differs across processes: %s != %s", i,
				s1.Tiles[i].ContentHash, s2.Tiles[i].ContentHash)
		}
	}
}

func TestForcedKillRecoversOnlyCommittedSnapshots(t *testing.T) {
	exe := buildCLI(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "kill.db")
	cmd := exec.Command(exe, "crash-demo", "--db", db, "--commits", "3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	store, err := OpenSnapshot(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("recovered rows after SIGKILL = %d, want 3 committed", len(rows))
	}
}

func readSummary(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
