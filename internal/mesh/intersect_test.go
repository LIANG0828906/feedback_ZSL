package mesh

import "testing"

func TestTrianglesIntersectCrossing(t *testing.T) {
	m := Mesh{
		Vertices: []Vec3{
			{X: -1, Y: 0, Z: 0},
			{X: 1, Y: 0, Z: 0},
			{X: 0, Y: 0, Z: 1},
			{X: 0, Y: -1, Z: 0.2},
			{X: 0, Y: 1, Z: 0.2},
			{X: 0, Y: 0, Z: -0.2},
		},
		Faces: []Face{{A: 0, B: 1, C: 2}, {A: 3, B: 4, C: 5}},
	}
	if !trianglesIntersect(m, 0, 1) {
		t.Fatal("expected crossing triangles to intersect")
	}
	if got := CountSelfIntersections(m); got != 1 {
		t.Fatalf("CountSelfIntersections=%d, want 1", got)
	}
}
