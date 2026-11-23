package sample

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"textureworkbench/internal/tex"
)

func TestGeneratorIsDeterministicAndManifestIsValid(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	manifest1, err := Generate(first, 424242)
	if err != nil {
		t.Fatalf("Generate first: %v", err)
	}
	manifest2, err := Generate(second, 424242)
	if err != nil {
		t.Fatalf("Generate second: %v", err)
	}
	if len(manifest1.Samples) != len(manifest2.Samples) || len(manifest1.Samples) < 8 {
		t.Fatalf("unexpected samples: %d and %d", len(manifest1.Samples), len(manifest2.Samples))
	}
	manifestBytes1, err := os.ReadFile(filepath.Join(first, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes2, err := os.ReadFile(filepath.Join(second, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestBytes1, manifestBytes2) {
		t.Fatalf("manifest.json is not byte-deterministic")
	}
	for _, sample := range manifest1.Samples {
		a, err := os.ReadFile(filepath.Join(first, sample.File))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(second, sample.File))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("sample %s is not deterministic", sample.File)
		}
		img, err := tex.DecodeImageFile(filepath.Join(first, sample.File))
		if err != nil {
			t.Fatalf("decode %s: %v", sample.File, err)
		}
		if img.ContentHash != sample.ContentHash {
			t.Fatalf("sample %s hash mismatch", sample.File)
		}
	}
	for _, descriptor := range manifest1.Descriptors {
		if err := descriptor.Validate(); err != nil {
			t.Fatalf("descriptor %s invalid: %v", descriptor.ID, err)
		}
		loadedDescriptor, err := tex.LoadDescriptorFile(filepath.Join(first, "descriptors", descriptor.ID+".json"))
		if err != nil {
			t.Fatalf("load descriptor %s: %v", descriptor.ID, err)
		}
		if loadedDescriptor.Version() != descriptor.Version() {
			t.Fatalf("descriptor %s file version mismatch", descriptor.ID)
		}
	}
	loaded, err := LoadManifest(first)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if loaded.Seed != 424242 || len(loaded.Descriptors) != len(manifest1.Descriptors) {
		t.Fatalf("manifest round trip mismatch")
	}
}
