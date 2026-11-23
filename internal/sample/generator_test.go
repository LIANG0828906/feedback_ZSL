package sample_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"mocapworkbench/internal/loader"
	"mocapworkbench/internal/sample"
)

func TestGenerateIsDeterministicAndValid(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	manifestA, err := sample.Generate(dirA, 4208691)
	if err != nil {
		t.Fatal(err)
	}
	manifestB, err := sample.Generate(dirB, 4208691)
	if err != nil {
		t.Fatal(err)
	}
	if manifestA.Seed != manifestB.Seed || len(manifestA.Files) != len(manifestB.Files) {
		t.Fatalf("manifests differ: %#v %#v", manifestA, manifestB)
	}
	for i := range manifestA.Files {
		if !reflect.DeepEqual(manifestA.Files[i], manifestB.Files[i]) {
			t.Fatalf("manifest entry %d differs", i)
		}
		rel := manifestA.Files[i].Path
		hashA := fileHash(t, filepath.Join(dirA, filepath.FromSlash(rel)))
		hashB := fileHash(t, filepath.Join(dirB, filepath.FromSlash(rel)))
		if hashA != hashB {
			t.Fatalf("%s is not deterministic: %s != %s", rel, hashA, hashB)
		}
	}
	if err := loader.VerifyManifest(filepath.Join(dirA, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	clips, err := filepath.Glob(filepath.Join(dirA, "clips", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(clips)
	if len(clips) != 3 {
		t.Fatalf("expected 3 clips, got %d", len(clips))
	}
	for _, path := range clips {
		clip, err := loader.LoadClip(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if clip.Truth.FrameCount != len(clip.Frames) {
			t.Fatalf("%s: truth frame count mismatch", path)
		}
	}
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
