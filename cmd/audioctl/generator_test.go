package main

import (
	"os"
	"path/filepath"
	"testing"

	"audiobus/internal/loader"
)

func TestGenerateSampleDeterministicAndLoadable(t *testing.T) {
	dir1 := filepath.Join(t.TempDir(), "one")
	dir2 := filepath.Join(t.TempDir(), "two")
	if err := GenerateSample(dir1, 42); err != nil {
		t.Fatal(err)
	}
	if err := GenerateSample(dir2, 42); err != nil {
		t.Fatal(err)
	}
	truth1, err := os.ReadFile(filepath.Join(dir1, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	truth2, err := os.ReadFile(filepath.Join(dir2, "truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(truth1) != string(truth2) {
		t.Fatal("fixed seed generator produced different truth")
	}
	set, err := loader.LoadSampleSet(dir1)
	if err != nil {
		t.Fatal(err)
	}
	if err := loader.CrossBranchEquivalent(set); err != nil {
		t.Fatal(err)
	}
}
