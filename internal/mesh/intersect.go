package mesh

import "math"

// SelfIntersectingPairs reports disjoint-vertex triangle pairs whose interiors
// or planes overlap under SAT. Adjacent faces sharing vertices are excluded so
// ordinary shared edges do not inflate the defect count.
func SelfIntersectingPairs(m *Mesh) [][2]int {
	var pairs [][2]int
	for i := 0; i < len(m.Faces); i++ {
		for j := i + 1; j < len(m.Faces); j++ {
			if facesShareVertex(m.Faces[i], m.Faces[j]) {
				continue
			}
			if TrianglesIntersect(m, i, j) {
				pairs = append(pairs, [2]int{i, j})
			}
		}
	}
	return pairs
}

func facesShareVertex(a, b Face) bool {
	for _, x := range a.Indices() {
		for _, y := range b.Indices() {
			if x == y {
				return true
			}
		}
	}
	return false
}

func TrianglesIntersect(m *Mesh, ia, ib int) bool {
	fa, fb := m.Faces[ia], m.Faces[ib]
	a := [3]Vec3{m.Vertices[fa.A], m.Vertices[fa.B], m.Vertices[fa.C]}
	b := [3]Vec3{m.Vertices[fb.A], m.Vertices[fb.B], m.Vertices[fb.C]}

	edgesA := [3]Vec3{Sub(a[1], a[0]), Sub(a[2], a[1]), Sub(a[0], a[2])}
	edgesB := [3]Vec3{Sub(b[1], b[0]), Sub(b[2], b[1]), Sub(b[0], b[2])}
	axes := make([]Vec3, 0, 11)
	if n := Normalized(Cross(edgesA[0], edgesA[1])); Norm(n) > 1e-12 {
		axes = append(axes, n)
	}
	if n := Normalized(Cross(edgesB[0], edgesB[1])); Norm(n) > 1e-12 {
		axes = append(axes, n)
	}
	for _, ea := range edgesA {
		for _, eb := range edgesB {
			c := Cross(ea, eb)
			if Norm(c) <= 1e-12 {
				continue
			}
			axes = append(axes, Scale(c, 1/Norm(c)))
		}
	}

	for _, axis := range axes {
		minA, maxA := math.Inf(1), math.Inf(-1)
		minB, maxB := math.Inf(1), math.Inf(-1)
		for _, p := range a {
			d := Dot(p, axis)
			minA = math.Min(minA, d)
			maxA = math.Max(maxA, d)
		}
		for _, p := range b {
			d := Dot(p, axis)
			minB = math.Min(minB, d)
			maxB = math.Max(maxB, d)
		}
		epsilon := 1e-9
		if maxA < minB-epsilon || maxB < minA-epsilon {
			return false
		}
	}
	return true
}
