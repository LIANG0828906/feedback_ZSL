package sample

import (
	"os"
	"path/filepath"
	"testing"

	"virtualstudio/formats"
	"virtualstudio/model"
)

func TestGenerateIsDeterministic(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	manifestA, err := Generate(dirA, 42)
	if err != nil {
		t.Fatal(err)
	}
	manifestB, err := Generate(dirB, 42)
	if err != nil {
		t.Fatal(err)
	}
	if manifestA.SceneHashes["v1"] != manifestB.SceneHashes["v1"] ||
		manifestA.SceneHashes["v2"] != manifestB.SceneHashes["v2"] ||
		manifestA.SceneHashes["v3"] != manifestB.SceneHashes["v3"] {
		t.Fatal("same seed produced different descriptor hashes")
	}
	for _, rel := range []string{"v1/scene.json", "v2/scene.json", "v3/scene.json", "commands.json", "truth.json", "manifest.json"} {
		a, err := os.ReadFile(filepath.Join(dirA, rel))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dirB, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("%s differs for the same seed", rel)
		}
	}
}

func TestTruthMatchesAllVersions(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(dir, 42); err != nil {
		t.Fatal(err)
	}
	truth, err := LoadTruth(filepath.Join(dir, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range formats.SupportedVersions {
		scene, err := formats.LoadScene(filepath.Join(dir, version, "scene.json"))
		if err != nil {
			t.Fatal(err)
		}
		commands, err := formats.LoadCommands(filepath.Join(dir, "commands.json"), scene)
		if err != nil {
			t.Fatal(err)
		}
		mismatch, err := TruthMismatch(scene, commands, truth)
		if err != nil {
			t.Fatal(err)
		}
		if mismatch != nil {
			t.Fatalf("%s: %v", version, mismatch)
		}
		if got := truth.LayerVisibility["character"].LastVisible; got != 15 {
			t.Fatalf("character endpoint=%d", got)
		}
	}
}

func TestDifferentSeedChangesPixels(t *testing.T) {
	a, err := CanonicalScene(42)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalScene(43)
	if err != nil {
		t.Fatal(err)
	}
	same := true
	for i := range a.Layers {
		if model.HashBytes(a.Layers[i].Pixels) != model.HashBytes(b.Layers[i].Pixels) {
			same = false
		}
	}
	if same {
		t.Fatal("different seeds produced identical layers")
	}
}
