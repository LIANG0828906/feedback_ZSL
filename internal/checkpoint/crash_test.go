package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"quota-cluster/internal/model"
)

func TestCrashHelper(t *testing.T) {
	if os.Getenv("QW_CRASH_HELPER") != "1" {
		t.Skip("crash helper only runs under subprocess environment")
	}
	path := os.Getenv("QW_CRASH_PATH")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cp := &Checkpoint{
		FormatVersion: model.FormatVersion, ProjectID: "crash-project", ProjectHash: "hash-old",
		CommittedTick: 2,
		TenantStates:  []model.TenantSnapshot{{TenantID: "tenant-new", UsedBytes: 2}},
	}
	_ = st.Save(cp, SaveOptions{ProjectHash: "hash-new", BeforeGlobalTick: func() {
		os.Exit(42)
	}})
}

func TestForcedCrashBeforeGlobalTickRecoversLastCommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.sqlite")
	old := &Checkpoint{
		FormatVersion: model.FormatVersion, ProjectID: "crash-project", ProjectHash: "hash-old",
		CommittedTick: 1,
		TenantStates:  []model.TenantSnapshot{{TenantID: "tenant-old", UsedBytes: 1}},
	}
	if err := SaveCheckpoint(path, old, SaveOptions{ProjectHash: "hash-old"}); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCrashHelper$")
	cmd.Env = append(os.Environ(), "QW_CRASH_HELPER=1", "QW_CRASH_PATH="+path)
	err = cmd.Run()
	if err == nil {
		t.Fatal("expected crash helper to exit non-zero")
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 42 {
		t.Fatalf("expected exit code 42, got %v", err)
	}

	loaded, err := LoadCheckpoint(path, "hash-old")
	if err != nil {
		t.Fatalf("reopen after forced crash: %v", err)
	}
	if loaded.CommittedTick != 1 {
		t.Fatalf("recovery read tick %d, want last committed tick 1", loaded.CommittedTick)
	}
	if len(loaded.TenantStates) != 1 || loaded.TenantStates[0].TenantID != "tenant-old" {
		t.Fatalf("half-written tenant state became visible: %+v", loaded.TenantStates)
	}
}
