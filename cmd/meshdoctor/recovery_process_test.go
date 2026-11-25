package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type cliSummary struct {
	OK         bool     `json:"ok"`
	Committed  []string `json:"committed"`
	CacheHits  int      `json:"cache_hits"`
	StopReason string   `json:"stop_reason"`
	Error      string   `json:"error"`
}

func TestProcessRecoveryBoundaries(t *testing.T) {
	root := repoRoot(t)
	binName := "meshdoctor"
	if runtime.GOOS == "windows" {
		binName = "meshdoctor.exe"
	}
	binPath := filepath.Join(t.TempDir(), binName)
	build := exec.Command("go", "build", "-o", binPath, "./cmd/meshdoctor")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	t.Run("stop_after_committed_stage", func(t *testing.T) {
		ws := t.TempDir()
		runCLI(t, binPath, ws, "generate", "--root", ws)
		first := runCLI(t, binPath, ws, "run", "--root", ws, "--sample", "tetra", "--db", "checkpoints/cancel.db", "--stop-after", "2")
		var summary cliSummary
		if err := json.Unmarshal(first, &summary); err != nil {
			t.Fatalf("decode %s: %v", first, err)
		}
		if len(summary.Committed) != 2 {
			t.Fatalf("committed=%v want 2", summary.Committed)
		}
		second := runCLI(t, binPath, ws, "run", "--root", ws, "--sample", "tetra", "--db", "checkpoints/cancel.db")
		if err := json.Unmarshal(second, &summary); err != nil {
			t.Fatalf("decode %s: %v", second, err)
		}
		if len(summary.Committed) != 6 || summary.CacheHits != 2 {
			t.Fatalf("resume summary=%+v want six stages and two hits", summary)
		}
	})

	t.Run("sigkill_before_smooth_then_resume", func(t *testing.T) {
		ws := t.TempDir()
		runCLI(t, binPath, ws, "generate", "--root", ws)
		ready := filepath.Join(ws, "ready")
		db := "checkpoints/kill.db"
		cmd := exec.Command(binPath, "run", "--root", ws, "--sample", "cube", "--db", db,
			"--hang-before", "smooth", "--ready-file", ready)
		cmd.Dir = ws
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if _, err := os.Stat(ready); err != nil {
			_ = cmd.Process.Kill()
			t.Fatal("ready file was not created before deadline")
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("force kill: %v", err)
		}
		_ = cmd.Wait()

		out := runCLI(t, binPath, ws, "run", "--root", ws, "--sample", "cube", "--db", db)
		var summary cliSummary
		if err := json.Unmarshal(out, &summary); err != nil {
			t.Fatalf("decode %s: %v", out, err)
		}
		if len(summary.Committed) != 6 {
			t.Fatalf("committed=%v want all six after recovery; error=%s", summary.Committed, summary.Error)
		}
		if summary.CacheHits != 4 {
			t.Fatalf("cache_hits=%d want four durable stages", summary.CacheHits)
		}
	})
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func runCLI(t *testing.T, bin string, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
	}
	return out
}
