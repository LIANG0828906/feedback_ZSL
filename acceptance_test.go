package featurekernel_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"featurekernel/internal/gen"
)

type cliSummary struct {
	GracefulStop bool `json:"graceful_stop"`
	Report       struct {
		StartCursor  int64 `json:"start_cursor"`
		CommittedSeq int64 `json:"committed_seq"`
	} `json:"report"`
	Windows   int `json:"windows"`
	Discarded int `json:"discarded"`
}

func TestFixedSeedOutputsAreByteIdentical(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	if _, err := gen.GenerateTree(first, gen.DefaultSeed, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := gen.GenerateTree(second, gen.DefaultSeed, 2); err != nil {
		t.Fatal(err)
	}

	files := []string{}
	if err := filepath.Walk(first, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		relative, err := filepath.Rel(first, path)
		if err != nil {
			return err
		}
		files = append(files, relative)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no generated files")
	}
	for _, relative := range files {
		a, err := os.ReadFile(filepath.Join(first, relative))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(second, relative))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("generated file differs for same seed: %s", relative)
		}
	}
}

func TestCLIGracefulSignalAndForcedCrashRecovery(t *testing.T) {
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "featurectl")
	build := exec.Command("go", "build", "-o", binary, "./cmd/featurectl")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, output)
	}

	dataDir := filepath.Join(t.TempDir(), "data")
	if _, err := gen.GenerateTree(dataDir, gen.DefaultSeed, 1); err != nil {
		t.Fatal(err)
	}
	sampleV3 := filepath.Join(dataDir, "samples", "batch-001.v3.json")
	truthData, err := os.ReadFile(filepath.Join(dataDir, "truth", "batch-001.truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var truth gen.TruthFile
	if err := json.Unmarshal(truthData, &truth); err != nil {
		t.Fatal(err)
	}

	t.Run("graceful_sigterm_keeps_commit_point", func(t *testing.T) {
		db := filepath.Join(t.TempDir(), "graceful.sqlite")
		summary := runCLISummary(t, binary, "consume", "--db", db, "--sample", sampleV3, "--batch-size", "1", "--term-after-commit", "12")
		if !summary.GracefulStop {
			t.Fatalf("expected graceful_stop summary: %+v", summary)
		}
		if summary.Report.CommittedSeq != 12 {
			t.Fatalf("committed seq = %d", summary.Report.CommittedSeq)
		}

		resume := runCLISummary(t, binary, "consume", "--db", db, "--sample", sampleV3, "--batch-size", "5")
		if resume.Report.StartCursor != 12 {
			t.Fatalf("resume start = %d", resume.Report.StartCursor)
		}
		if resume.Windows != truth.Summary.WindowCount || resume.Discarded != truth.Summary.DiscardedRecordCount {
			t.Fatalf("resume summary = %+v truth=%+v", resume, truth.Summary)
		}
	})

	t.Run("sigkill_injection_rolls_unfinished_commit_forward", func(t *testing.T) {
		db := filepath.Join(t.TempDir(), "crash.sqlite")
		crash := exec.Command(binary, "consume", "--db", db, "--sample", sampleV3,
			"--target", "30", "--batch-size", "10", "--crash-point", "after_window_states")
		var stderr bytes.Buffer
		crash.Stderr = &stderr
		err := crash.Run()
		if err == nil {
			t.Fatal("forced crash command unexpectedly succeeded")
		}

		partial := runCLISummary(t, binary, "consume", "--db", db, "--sample", sampleV3,
			"--target", "20", "--batch-size", "5")
		if partial.Report.StartCursor != 0 || partial.Report.CommittedSeq != 20 {
			t.Fatalf("crashed transaction should leave cursor at 0, got %+v stderr=%s", partial.Report, stderr.String())
		}

		full := runCLISummary(t, binary, "consume", "--db", db, "--sample", sampleV3, "--batch-size", "7")
		if full.Report.StartCursor != 20 {
			t.Fatalf("second resume start = %d", full.Report.StartCursor)
		}
		if full.Windows != truth.Summary.WindowCount || full.Discarded != truth.Summary.DiscardedRecordCount {
			t.Fatalf("full recovery summary = %+v truth=%+v", full, truth.Summary)
		}
	})
}

func runCLISummary(t *testing.T, binary string, args ...string) cliSummary {
	t.Helper()
	cmd := exec.Command(binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("command %v failed: %v\nstdout=%s\nstderr=%s", args, err, stdout.String(), stderr.String())
	}
	var summary cliSummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("parse summary: %v\noutput=%s", err, stdout.String())
	}
	return summary
}
