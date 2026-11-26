package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProcessKillAndResume(t *testing.T) {
	root := "../.."
	bin := filepath.Join(t.TempDir(), "msdemo")
	build := exec.Command("go", "build", "-o", bin, "./cmd/msdemo")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}

	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	crash := exec.Command(bin, "crash", "--state", state, "--after", "5")
	if err := crash.Run(); err == nil {
		t.Fatal("expected crash subprocess to be killed")
	}

	resume := exec.Command(bin, "resume", "--state", state)
	if out, err := resume.CombinedOutput(); err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	full := exec.Command(bin, "full-run", "--state", state)
	if out, err := full.CombinedOutput(); err != nil {
		t.Fatalf("full run: %v\n%s", err, out)
	}
	compare := exec.Command(bin, "compare-reports",
		"-want", filepath.Join(state, "full-report.json"),
		"-got", filepath.Join(state, "resume-report.json"))
	if out, err := compare.CombinedOutput(); err != nil {
		t.Fatalf("compare reports: %v\n%s", err, out)
	}
}
