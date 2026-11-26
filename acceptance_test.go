package quotakernel_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"quotakernel/accept"
	"quotakernel/checkpoint"
	"quotakernel/generate"
	"quotakernel/sampleio"
)

func TestAcceptanceReportFromGeneratedSamples(t *testing.T) {
	seed := int64(42)
	sampleDir := filepath.Join(t.TempDir(), "samples")
	checkpointDir := filepath.Join(t.TempDir(), "checkpoint")

	bundle, err := generate.Generate(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := sampleio.WriteBundle(sampleDir, bundle); err != nil {
		t.Fatal(err)
	}
	report, err := accept.VerifyDir(sampleDir, checkpointDir)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		for _, check := range report.Checks {
			if !check.OK {
				t.Errorf("check %s failed: %s", check.Name, check.Detail)
			}
		}
		t.Fatal("acceptance report not OK")
	}
}

func TestCLIForcedKillThenRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI crash test in short mode")
	}
	root := repoRoot(t)
	binary := filepath.Join(t.TempDir(), "quotaaccept")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/quotaaccept")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}

	sampleDir := filepath.Join(t.TempDir(), "samples")
	crashDir := filepath.Join(t.TempDir(), "crash-checkpoint")
	generateCmd := exec.Command(binary, "generate", "--seed", "42", "--dir", sampleDir)
	if out, err := generateCmd.CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, out)
	}

	crash := exec.Command(binary, "run", "--seed", "42", "--checkpoint", crashDir, "--kill-at-tick", "4")
	var crashErr bytes.Buffer
	crash.Stderr = &crashErr
	err := crash.Run()
	if err == nil {
		t.Fatal("expected forced kill to terminate child unsuccessfully")
	}
	if !strings.Contains(crashErr.String(), "sending SIGKILL") {
		t.Fatalf("crash hook did not run; stderr=%s", crashErr.String())
	}

	metaData, err := os.ReadFile(filepath.Join(crashDir, "committed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta checkpoint.Metadata
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.CommittedTick != 3 {
		t.Fatalf("global committed tick=%d, want 3", meta.CommittedTick)
	}
	orphan := filepath.Join(crashDir, "ticks", "000000000004.entities.json")
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("expected durable orphan entity at %s: %v", orphan, err)
	}

	recoverReport := filepath.Join(t.TempDir(), "recovery.json")
	recoverCmd := exec.Command(binary, "recover", "--dir", sampleDir, "--checkpoint", crashDir, "--report", recoverReport)
	if out, err := recoverCmd.CombinedOutput(); err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	recoveredData, err := os.ReadFile(recoverReport)
	if err != nil {
		t.Fatal(err)
	}
	var recovery map[string]any
	if err := json.Unmarshal(recoveredData, &recovery); err != nil {
		t.Fatal(err)
	}
	if recovery["ok"] != true {
		t.Fatalf("recovery report not ok: %s", recoveredData)
	}
}

func TestCLIGracefulSignalWritesReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping signal test in short mode")
	}
	root := repoRoot(t)
	binary := filepath.Join(t.TempDir(), "quotaaccept-signal")
	build := exec.Command("go", "build", "-o", binary, "./cmd/quotaaccept")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}

	checkpointDir := filepath.Join(t.TempDir(), "checkpoint")
	exitReport := filepath.Join(t.TempDir(), "exit-report.json")
	cmd := exec.Command(binary, "run", "--checkpoint", checkpointDir, "--tick-delay", "5s", "--exit-report", exitReport)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful interrupt should exit 0: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process did not stop after SIGINT")
	}
	data, err := os.ReadFile(exitReport)
	if err != nil {
		t.Fatalf("graceful exit report was not written: %v", err)
	}
	if !strings.Contains(string(data), "graceful-signal") {
		t.Fatalf("unexpected exit report: %s", data)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
		return wd
	}
	return filepath.Dir(wd)
}
