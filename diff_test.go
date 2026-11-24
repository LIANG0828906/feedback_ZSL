package terrain

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDiffMeshLocatesFirstVertexField(t *testing.T) {
	project := GenerateSample(42)
	compiled, err := CompileProject(project)
	if err != nil {
		t.Fatal(err)
	}
	original := compiled.Meshes[0]
	mutated := cloneMesh(original)
	mutated.Vertices[3].Y += 0.37

	err = DiffMesh(7, original, mutated)
	var diff *DiffError
	if !errors.As(err, &diff) {
		t.Fatalf("expected DiffError, got %T %v", err, err)
	}
	if diff.Location.BlockNo != 7 {
		t.Fatalf("block=%d", diff.Location.BlockNo)
	}
	if diff.Location.VertexIndex == nil || *diff.Location.VertexIndex != 3 {
		t.Fatalf("vertex index=%v", diff.Location.VertexIndex)
	}
	if diff.Location.Field != "y" {
		t.Fatalf("field=%s", diff.Location.Field)
	}
	if !IsDiffError(&DiffError{Location: DiffLocation{BlockNo: 1}}) {
		t.Fatal("IsDiffError should recognize pointer value")
	}
}

func cloneMesh(m *Mesh) *Mesh {
	data, _ := json.Marshal(m)
	var out Mesh
	_ = json.Unmarshal(data, &out)
	return &out
}
