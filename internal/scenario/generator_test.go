package scenario_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"obsproj/internal/runner"
	"obsproj/internal/scenario"
	"obsproj/internal/schema"
)

func TestFixedSeedIsByteDeterministic(t *testing.T) {
	first, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.V1, second.V1) ||
		!bytes.Equal(first.V2, second.V2) ||
		!bytes.Equal(first.V3, second.V3) {
		t.Fatal("fixed-seed schema artifacts are not byte-identical")
	}
	if first.Session.Fingerprint() != second.Session.Fingerprint() {
		t.Fatal("fixed-seed semantic hashes differ")
	}
	if first.Truth.SemanticHash != second.Truth.SemanticHash {
		t.Fatal("fixed-seed truth hashes differ")
	}
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	if err := scenario.WriteBundle(dir1, first); err != nil {
		t.Fatal(err)
	}
	if err := scenario.WriteBundle(dir2, second); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"truth.json", "manifest.json"} {
		left, err := os.ReadFile(filepath.Join(dir1, name))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(dir2, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("%s is not deterministic", name)
		}
	}
}

func TestTruthMatchesReplay(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.RunFullReplay(bundle.Session, runner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Commits) != len(bundle.Truth.Windows) {
		t.Fatalf("windows=%d truth=%d", len(result.Commits), len(bundle.Truth.Windows))
	}
	var decisions []string
	for i, commit := range result.Commits {
		truthWindow := bundle.Truth.Windows[i]
		if commit.Window != truthWindow.Window ||
			commit.At != truthWindow.At ||
			commit.SourceRegion != truthWindow.SourceRegion ||
			commit.TopologyVersion != truthWindow.TopologyVersion {
			t.Fatalf("window %d metadata differs from truth", commit.Window)
		}
		if !equalStrings(commit.ActiveScreens, truthWindow.ActiveScreens) {
			t.Fatalf("window %d active set differs", commit.Window)
		}
		if len(commit.Screens) != len(truthWindow.Screens) {
			t.Fatalf("window %d screen count differs", commit.Window)
		}
		for j, screen := range commit.Screens {
			truthScreen := truthWindow.Screens[j]
			if screen.ScreenID != truthScreen.ScreenID ||
				screen.Mapped.RawPixels != truthScreen.RawPixels ||
				screen.Mapped.Pixels != truthScreen.Pixels ||
				screen.Mapped.Params != truthScreen.Params ||
				screen.Metrics != truthScreen.Metrics ||
				screen.ContentHash != truthScreen.ContentHash {
				t.Fatalf("window %d screen %s differs from truth", commit.Window, screen.ScreenID)
			}
			if len(screen.Packets) != len(truthScreen.Packets) {
				t.Fatalf("window %d screen %s packet count differs", commit.Window, screen.ScreenID)
			}
			for k := range screen.Packets {
				if screen.Packets[k] != truthScreen.Packets[k] {
					t.Fatalf("window %d screen %s packet %d differs", commit.Window, screen.ScreenID, k)
				}
			}
		}
		for _, decision := range commit.Decisions {
			decisions = append(decisions, decision.Winner)
		}
	}
	if len(decisions) != len(bundle.Truth.Decisions) {
		t.Fatalf("decisions=%d truth=%d", len(decisions), len(bundle.Truth.Decisions))
	}
	if !equalStrings(result.Final.ActiveScreens, bundle.Truth.Final.ActiveScreens) {
		t.Fatal("final active set differs from truth")
	}
}

func TestWriteBundleArtifactsReload(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := scenario.WriteBundle(dir, bundle); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session_v1.json", "session_v2.json", "session_v3.json", "truth.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"session_v1.json", "session_v2.json", "session_v3.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := schema.Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if loaded.Fingerprint() != bundle.Session.Fingerprint() {
			t.Fatalf("%s changed semantic model", name)
		}
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
