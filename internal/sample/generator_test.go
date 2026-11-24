package sample_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"ocrworkbench/internal/sample"
)

func TestGeneratorIsByteDeterministicForFixedSeed(t *testing.T) {
	leftDir := t.TempDir()
	rightDir := t.TempDir()
	left, err := sample.Generate(sample.Options{OutputDir: leftDir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	right, err := sample.Generate(sample.Options{OutputDir: rightDir, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatal("manifests differ for the same seed")
	}
	for _, image := range left.Images {
		leftBytes, err := os.ReadFile(filepath.Join(leftDir, image.File))
		if err != nil {
			t.Fatal(err)
		}
		rightBytes, err := os.ReadFile(filepath.Join(rightDir, image.File))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(leftBytes, rightBytes) {
			t.Fatalf("%s differs for the same seed", image.File)
		}
	}
	other, err := sample.Generate(sample.Options{OutputDir: t.TempDir(), Seed: 43})
	if err != nil {
		t.Fatal(err)
	}
	if other.TruthFingerprint == left.TruthFingerprint {
		t.Fatal("different seeds produced the same layout truth")
	}
}
