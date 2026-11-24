package samples_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"economycore/internal/core"
	"economycore/internal/samples"
)

func TestGeneratorIsByteDeterministic(t *testing.T) {
	profiles := []samples.Profile{{ID: "tiny", Intents: 180, Horizon: 360, TruthSamples: 6}}
	a := t.TempDir()
	b := t.TempDir()
	if _, err := samples.Generate(a, 99, profiles); err != nil {
		t.Fatal(err)
	}
	if _, err := samples.Generate(b, 99, profiles); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "tiny.json"} {
		left, err := os.ReadFile(filepath.Join(a, name))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(b, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("%s is not deterministic across runs", name)
		}
	}
}

func TestGeneratedTruthMatchesCore(t *testing.T) {
	dir := t.TempDir()
	if _, err := samples.Generate(dir, 7, []samples.Profile{{
		ID: "truth", Intents: 220, Horizon: 400, TruthSamples: 8,
	}}); err != nil {
		t.Fatal(err)
	}
	scene, err := samples.Load(dir, "truth")
	if err != nil {
		t.Fatal(err)
	}
	ticks := append([]int64(nil), scene.Truth.SampleTicks...)
	run, err := core.RunFull(scene, ticks)
	if err != nil {
		t.Fatal(err)
	}
	byTick := make(map[int64]core.Snapshot, len(run.Snapshots))
	for _, snap := range run.Snapshots {
		byTick[snap.Tick] = snap
	}
	for _, expected := range scene.Truth.Snapshots {
		actual, ok := byTick[expected.Tick]
		if !ok {
			t.Fatalf("missing tick %d", expected.Tick)
		}
		if diff := core.CompareTruth(expected, actual); diff != nil {
			t.Fatalf("truth diverged: %v", diff)
		}
	}
}
