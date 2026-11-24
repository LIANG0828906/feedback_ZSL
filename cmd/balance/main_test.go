package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIEndToEnd(t *testing.T) {
	dir := t.TempDir()
	projectPath := filepath.Join(dir, "project.json")
	fullPath := filepath.Join(dir, "full.json")
	realtimePath := filepath.Join(dir, "realtime.json")
	partialPath := filepath.Join(dir, "partial.json")
	resumePath := filepath.Join(dir, "resumed.json")
	checkpointPath := filepath.Join(dir, "checkpoint.json")

	if code, out, errOut := invoke(t, "generate", "--out", projectPath, "--seed", "42"); code != 0 {
		t.Fatalf("generate failed: code=%d out=%s err=%s", code, out, errOut)
	}
	if _, err := os.Stat(projectPath); err != nil {
		t.Fatalf("generated project is missing: %v", err)
	}
	if code, out, errOut := invoke(t, "validate", "--project", projectPath); code != 0 || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("validate failed: code=%d out=%s err=%s", code, out, errOut)
	}

	if code, out, errOut := invoke(t, "run", "--project", projectPath, "--mode", "full", "--report", fullPath); code != 0 {
		t.Fatalf("full run failed: code=%d out=%s err=%s", code, out, errOut)
	}
	if code, out, errOut := invoke(t, "run", "--project", projectPath, "--mode", "realtime", "--state", filepath.Join(dir, "realtime-checkpoint.json"), "--report", realtimePath); code != 0 {
		t.Fatalf("realtime run failed: code=%d out=%s err=%s", code, out, errOut)
	}
	if code, out, errOut := invoke(t, "run", "--project", projectPath, "--mode", "realtime", "--state", checkpointPath,
		"--stop-after", "2", "--report", partialPath); code != 0 {
		t.Fatalf("partial run failed: code=%d out=%s err=%s", code, out, errOut)
	}
	if code, out, errOut := invoke(t, "run", "--project", projectPath, "--mode", "resume", "--state", checkpointPath, "--report", resumePath); code != 0 {
		t.Fatalf("resume run failed: code=%d out=%s err=%s", code, out, errOut)
	}
	if code, out, errOut := invoke(t, "compare", "--expected", fullPath, "--actual", resumePath); code != 0 || !strings.Contains(out, `"equal": true`) {
		t.Fatalf("compare failed: code=%d out=%s err=%s", code, out, errOut)
	}
	if code, out, errOut := invoke(t, "verify", "--project", projectPath, "--state", checkpointPath); code != 0 || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("verify failed: code=%d out=%s err=%s", code, out, errOut)
	}

	injectedReport := filepath.Join(dir, "injected.json")
	code, out, errOut := invoke(t, "run", "--project", projectPath, "--mode", "full", "--report", injectedReport,
		"--inject", "resource", "--inject-plan", "baseline", "--inject-scenario", "duel",
		"--inject-match", "1", "--inject-turn", "2")
	if code == 0 || !strings.Contains(out, "E_RES") || !strings.Contains(errOut, "E_RES") {
		t.Fatalf("fault injection did not return E_RES: code=%d out=%s err=%s", code, out, errOut)
	}
	if _, err := os.Stat(injectedReport); err != nil {
		t.Fatalf("failed run did not preserve partial report: %v", err)
	}

	code, out, errOut = invoke(t, "run", "--project", projectPath, "--mode", "full", "--budget-max-total", "3")
	if code == 0 || !strings.Contains(out, "E_BUDGET") {
		t.Fatalf("budget override did not fail with E_BUDGET: code=%d out=%s err=%s", code, out, errOut)
	}
}

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runCLI(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}
