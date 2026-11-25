package main_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCLIForceKillThenResumeAndReview(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI crash integration in short mode")
	}
	root := t.TempDir()
	exeName := "haptic-test-bin"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	binary := filepath.Join(root, exeName)

	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	samples := filepath.Join(root, "samples")
	db := filepath.Join(root, "crash.db")
	gen := exec.Command(binary, "generate", "--dir", samples)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, out)
	}

	crashed := exec.Command(binary, "run", "--samples", samples, "--out", root, "--db", db, "--hang-after", "2")
	stdout, err := crashed.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	crashed.Stderr = &stderr
	if err := crashed.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "hang_ready") {
				ready <- line
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(60 * time.Second):
		_ = crashed.Process.Kill()
		t.Fatalf("process did not reach durable hang point; stderr=%s", stderr.String())
	}
	if err := crashed.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = crashed.Wait()

	resume := exec.Command(binary, "run", "--samples", samples, "--out", root, "--db", db, "--resume")
	out, err := resume.CombinedOutput()
	if err != nil {
		t.Fatalf("resume after kill: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"loaded_checkpoint": 2`) || !strings.Contains(string(out), `"packets": 5`) {
		t.Fatalf("unexpected resume output: %s", out)
	}

	review := exec.Command(binary, "review", "--db", db, "--truth", filepath.Join(samples, "truth.json"))
	reviewOut, err := review.CombinedOutput()
	if err != nil {
		t.Fatalf("review after kill: %v\n%s", err, reviewOut)
	}
	if !strings.Contains(string(reviewOut), `"ok": true`) {
		t.Fatalf("review did not pass: %s", reviewOut)
	}
}
