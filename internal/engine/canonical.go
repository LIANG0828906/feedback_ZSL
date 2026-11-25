package engine

import (
	"fmt"
	"sort"

	"terrainpatch/internal/boundary"
	"terrainpatch/internal/model"
)

type CanonVertex struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

type CanonTriangle struct {
	A CanonVertex `json:"a"`
	B CanonVertex `json:"b"`
	C CanonVertex `json:"c"`
}

type CanonTriangleError struct {
	Triangle    CanonTriangle `json:"triangle"`
	MaxAbsError int64         `json:"maxAbsError"`
	L1Error     int64         `json:"l1Error"`
}

type CanonMesh struct {
	TileID    string               `json:"tileId"`
	Depth     int                  `json:"depth"`
	Vertices  []CanonVertex        `json:"vertices"`
	Triangles []CanonTriangle      `json:"triangles"`
	Errors    []CanonTriangleError `json:"errors"`
}

func CanonicalMesh(m *model.Mesh) CanonMesh {
	if m == nil {
		return CanonMesh{}
	}
	byID := map[int]model.Vertex{}
	vertices := make([]CanonVertex, 0, len(m.Vertices))
	for _, v := range m.Vertices {
		byID[v.ID] = v
		cv := CanonVertex{X: v.X, Y: v.Y, Z: v.Z}
		vertices = append(vertices, cv)
	}
	sort.Slice(vertices, func(i, j int) bool {
		if vertices[i].X != vertices[j].X {
			return vertices[i].X < vertices[j].X
		}
		return vertices[i].Y < vertices[j].Y
	})

	tris := make([]CanonTriangle, 0, len(m.Triangles))
	for _, t := range m.Triangles {
		vs := []CanonVertex{canon(byID[t.V0]), canon(byID[t.V1]), canon(byID[t.V2])}
		sort.Slice(vs, func(i, j int) bool { return canonLess(vs[i], vs[j]) })
		tris = append(tris, CanonTriangle{A: vs[0], B: vs[1], C: vs[2]})
	}
	sort.Slice(tris, func(i, j int) bool { return triLess(tris[i], tris[j]) })

	errByTri := map[int]model.TriangleError{}
	for _, te := range m.TriangleErrors {
		errByTri[te.TriangleID] = te
	}
	errs := make([]CanonTriangleError, 0, len(m.Triangles))
	for _, t := range m.Triangles {
		vs := []CanonVertex{canon(byID[t.V0]), canon(byID[t.V1]), canon(byID[t.V2])}
		sort.Slice(vs, func(i, j int) bool { return canonLess(vs[i], vs[j]) })
		ct := CanonTriangle{A: vs[0], B: vs[1], C: vs[2]}
		te := errByTri[t.ID]
		errs = append(errs, CanonTriangleError{Triangle: ct, MaxAbsError: te.MaxAbsError, L1Error: te.L1Error})
	}
	sort.Slice(errs, func(i, j int) bool { return triLess(errs[i].Triangle, errs[j].Triangle) })
	return CanonMesh{TileID: m.TileID, Depth: m.Depth, Vertices: vertices, Triangles: tris, Errors: errs}
}

func canon(v model.Vertex) CanonVertex {
	return CanonVertex{X: v.X, Y: v.Y, Z: v.Z}
}

func canonLess(a, b CanonVertex) bool {
	if a.X != b.X {
		return a.X < b.X
	}
	if a.Y != b.Y {
		return a.Y < b.Y
	}
	return a.Z < b.Z
}

func triLess(a, b CanonTriangle) bool {
	av := [3]CanonVertex{a.A, a.B, a.C}
	bv := [3]CanonVertex{b.A, b.B, b.C}
	for i := 0; i < 3; i++ {
		if canonLess(av[i], bv[i]) {
			return true
		}
		if canonLess(bv[i], av[i]) {
			return false
		}
	}
	return false
}

type Difference struct {
	TileID   string `json:"tileId,omitempty"`
	Boundary string `json:"boundaryId,omitempty"`
	Field    string `json:"field"`
	Index    int    `json:"index,omitempty"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
}

func (d Difference) Error() string {
	return fmt.Sprintf("%s first mismatch %s: expected=%#v actual=%#v", d.TileID, d.Field, d.Expected, d.Actual)
}

func CompareCanonicalMesh(tileID string, expected, actual *model.Mesh) *Difference {
	e := CanonicalMesh(expected)
	a := CanonicalMesh(actual)
	if e.Depth != a.Depth {
		return &Difference{TileID: tileID, Field: "depth", Expected: e.Depth, Actual: a.Depth}
	}
	if len(e.Vertices) != len(a.Vertices) {
		return &Difference{TileID: tileID, Field: "vertices.length", Expected: len(e.Vertices), Actual: len(a.Vertices)}
	}
	for i := range e.Vertices {
		if e.Vertices[i] != a.Vertices[i] {
			return &Difference{TileID: tileID, Field: "vertices.item", Index: i, Expected: e.Vertices[i], Actual: a.Vertices[i]}
		}
	}
	if len(e.Triangles) != len(a.Triangles) {
		return &Difference{TileID: tileID, Field: "triangles.length", Expected: len(e.Triangles), Actual: len(a.Triangles)}
	}
	for i := range e.Triangles {
		if e.Triangles[i] != a.Triangles[i] {
			return &Difference{TileID: tileID, Field: "triangles.item", Index: i, Expected: e.Triangles[i], Actual: a.Triangles[i]}
		}
	}
	if len(e.Errors) != len(a.Errors) {
		return &Difference{TileID: tileID, Field: "triangleErrors.length", Expected: len(e.Errors), Actual: len(a.Errors)}
	}
	for i := range e.Errors {
		if e.Errors[i] != a.Errors[i] {
			return &Difference{TileID: tileID, Field: "triangleErrors.item", Index: i, Expected: e.Errors[i], Actual: a.Errors[i]}
		}
	}
	return nil
}

func CompareBoundaryStat(expected, actual boundary.Stat) *Difference {
	if expected.MaxAngleMicroDeg != actual.MaxAngleMicroDeg {
		return &Difference{Boundary: expected.BoundaryID, Field: "maxAngleMicroDeg", Expected: expected.MaxAngleMicroDeg, Actual: actual.MaxAngleMicroDeg}
	}
	if expected.Category != actual.Category {
		return &Difference{Boundary: expected.BoundaryID, Field: "category", Expected: expected.Category, Actual: actual.Category}
	}
	if expected.Code != actual.Code {
		return &Difference{Boundary: expected.BoundaryID, Field: "code", Expected: expected.Code, Actual: actual.Code}
	}
	if expected.AlignmentMismatchCount != actual.AlignmentMismatchCount {
		return &Difference{Boundary: expected.BoundaryID, Field: "alignmentMismatchCount", Expected: expected.AlignmentMismatchCount, Actual: actual.AlignmentMismatchCount}
	}
	if expected.HeightErrorA != actual.HeightErrorA || expected.HeightErrorB != actual.HeightErrorB {
		return &Difference{Boundary: expected.BoundaryID, Field: "heightError", Expected: [2]int64{expected.HeightErrorA, expected.HeightErrorB}, Actual: [2]int64{actual.HeightErrorA, actual.HeightErrorB}}
	}
	return nil
}
