package generator_test

import (
	"bytes"
	"testing"

	"dialogueaudio/generator"
)

func TestBuildIsByteDeterministic(t *testing.T) {
	first, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Files) != len(second.Files) {
		t.Fatal("file sets differ")
	}
	for name, data := range first.Files {
		if !bytes.Equal(data, second.Files[name]) {
			t.Fatalf("generated file %s is not deterministic", name)
		}
	}
	if len(first.Truth.Clips) != 6 || len(first.Project.Clips) != 6 {
		t.Fatalf("unexpected sample shape: %d/%d", len(first.Truth.Clips), len(first.Project.Clips))
	}
}

func TestManifestRoundTrip(t *testing.T) {
	set, err := generator.Build(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := set.Write(dir); err != nil {
		t.Fatal(err)
	}
	if err := generator.ValidateManifest(dir); err != nil {
		t.Fatal(err)
	}
}
