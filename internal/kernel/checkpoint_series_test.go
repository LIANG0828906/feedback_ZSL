package kernel

import (
	"path/filepath"
	"testing"
)

func TestEveryCommittedTickCanResume(t *testing.T) {
	p := GenerateProject(42)
	full, err := RunReference(p, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "commit.sqlite")
	for _, tick := range full.Ticks {
		cp := BuildCheckpoint(p, DefaultBudgets(), tick)
		if err := SaveCheckpoint(path, cp); err != nil {
			t.Fatalf("save tick %d: %v", tick.Tick, err)
		}
		loaded, err := LoadCheckpoint(path)
		if err != nil {
			t.Fatalf("load tick %d: %v", tick.Tick, err)
		}
		if loaded.CommittedTick != tick.Tick {
			t.Fatalf("tick mismatch: saved %d loaded %d", tick.Tick, loaded.CommittedTick)
		}
		resumed, err := ResumeFromCheckpoint(p, DefaultBudgets(), loaded)
		if err != nil {
			t.Fatalf("resume tick %d: %v", tick.Tick, err)
		}
		if !reportMatchesFromTick(resumed, full, tick.Tick+1) {
			t.Fatalf("suffix after tick %d differs", tick.Tick)
		}
	}
}
