package kernel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteCheckpointRoundTripSmoke(t *testing.T) {
	bundle, err := GenerateBundle(42)
	if err != nil {
		t.Fatal(err)
	}
	tick := bundle.Truth.Ticks[len(bundle.Truth.Ticks)-1]
	cp := BuildCheckpoint(bundle.Project, DefaultBudgets(), tick)
	path := filepath.Join(t.TempDir(), "checkpoint.sqlite")
	if err := SaveCheckpoint(path, cp); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CommittedTick != cp.CommittedTick || loaded.ProjectFingerprint != cp.ProjectFingerprint {
		t.Fatalf("bad checkpoint: %+v", loaded)
	}
	if _, err := os.Stat(path + ".sql"); err != nil {
		t.Fatal(err)
	}
	t.Log(path)
}
