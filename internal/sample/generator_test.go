package sample_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"spectralforge/internal/domain"
	"spectralforge/internal/sample"
)

func TestGeneratorIsByteDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if _, err := sample.Generate(a, 424242); err != nil {
		t.Fatal(err)
	}
	if _, err := sample.Generate(b, 424242); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		domain.SceneFileName,
		domain.SpectralFileName,
		domain.TruthFileName,
		domain.ConfigFileName,
		"manifest.json",
	} {
		left, err := os.ReadFile(filepath.Join(a, name))
		if err != nil {
			t.Fatal(err)
		}
		right, err := os.ReadFile(filepath.Join(b, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(left, right) {
			t.Fatalf("%s differs between identical seeds", name)
		}
	}
}

func TestDifferentSeedsStillProduceValidAnalyticTruth(t *testing.T) {
	dir := t.TempDir()
	b1, err := sample.Generate(dir, 11)
	if err != nil {
		t.Fatal(err)
	}
	dir2 := t.TempDir()
	b2, err := sample.Generate(dir2, 12)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Truth.Source != "analytic_generator_v1" || b2.Truth.Source != "analytic_generator_v1" {
		t.Fatal("unexpected truth source")
	}
	if b1.Truth.SceneHash != b2.Truth.SceneHash {
		// Seed is part of scene content, so hashes must differ even though the
		// analytic construction is otherwise shared.
		return
	}
	t.Fatal("seed did not affect scene hash")
}
