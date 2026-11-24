package samplegen_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"volumelab/pkg/samplegen"
)

func TestGenerateIsByteDeterministic(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	a, err := samplegen.Generate(samplegen.Options{OutputDir: dirA, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	b, err := samplegen.Generate(samplegen.Options{OutputDir: dirB, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	manifestA, _ := os.ReadFile(a.ManifestPath)
	manifestB, _ := os.ReadFile(b.ManifestPath)
	if !bytes.Equal(manifestA, manifestB) {
		t.Fatal("manifests differ for the same seed")
	}
	for _, volume := range a.Volumes {
		name := filepath.Base(volume.File)
		fileA, err := os.ReadFile(filepath.Join(dirA, name))
		if err != nil {
			t.Fatal(err)
		}
		fileB, err := os.ReadFile(filepath.Join(dirB, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(fileA, fileB) {
			t.Fatalf("%s differs for the same seed", name)
		}
	}
}

func TestDifferentSeedChangesVoxelPayload(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	a, _ := samplegen.Generate(samplegen.Options{OutputDir: dirA, Seed: 42})
	_, _ = samplegen.Generate(samplegen.Options{OutputDir: dirB, Seed: 43})
	name := filepath.Base(a.Volumes[0].File)
	fileA, _ := os.ReadFile(filepath.Join(dirA, name))
	fileB, _ := os.ReadFile(filepath.Join(dirB, name))
	if bytes.Equal(fileA, fileB) {
		t.Fatal("different seeds produced identical volume data")
	}
}
