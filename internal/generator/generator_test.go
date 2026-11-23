package generator

import (
	"path/filepath"
	"reflect"
	"testing"

	"meshforge/internal/mesh"
)

func TestGenerateDeterministicAndTruthCounts(t *testing.T) {
	a, err := Generate(3, 991, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(3, 991, "a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Mesh.ContentHash() != b.Mesh.ContentHash() {
		t.Fatal("same seed produced different mesh content")
	}
	if !reflect.DeepEqual(a.Truth, b.Truth) {
		t.Fatal("same seed produced different truth")
	}
	if a.Truth.Original.Vertices != 258 || a.Truth.Original.Faces != 512 ||
		a.Truth.Original.Edges != 768 || a.Truth.Original.Euler != 2 {
		t.Fatalf("unexpected baseline truth: %+v", a.Truth.Original)
	}
	for _, fi := range append(append([]int{}, a.Truth.FlippedFaceIDs...), a.Truth.HoleFaceIDs...) {
		if fi < 0 || fi >= len(a.Mesh.Faces) {
			t.Fatalf("truth highlight face %d outside raw mesh", fi)
		}
	}
	orientation := mesh.AnalyzeOrientation(a.Mesh)
	expectedFlips := make(map[int]bool, len(a.Truth.FlippedFaceIDs))
	for _, fi := range a.Truth.FlippedFaceIDs {
		expectedFlips[fi] = true
	}
	for fi, flip := range orientation.FaceFlips {
		if flip != expectedFlips[fi] {
			t.Fatalf("orientation flip mismatch at face %d: got %t IDs=%v", fi, flip, a.Truth.FlippedFaceIDs)
		}
	}
	c, err := Generate(3, 992, "a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Mesh.ContentHash() == c.Mesh.ContentHash() {
		t.Fatal("different seeds should change the sample")
	}
	dir := t.TempDir()
	if err := SaveSample(dir, a); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSample(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Mesh.ContentHash() != a.Mesh.ContentHash() ||
		!reflect.DeepEqual(loaded.Truth, a.Truth) {
		t.Fatalf("sample round-trip mismatch at %s", filepath.Join(dir, "mesh.json"))
	}
}
