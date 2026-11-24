package terrain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
)

func CanonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func DigestBytes(prefix string, b []byte) string {
	sum := sha256.Sum256(append([]byte(prefix), b...))
	return prefix + ":" + hex.EncodeToString(sum[:])
}

func VertexDigest(vertices []Vertex) string {
	type compact struct {
		GX int     `json:"gx"`
		GY int     `json:"gy"`
		X  float64 `json:"x"`
		Y  float64 `json:"y"`
		Z  float64 `json:"z"`
	}
	items := make([]compact, len(vertices))
	for i, v := range vertices {
		items[i] = compact{GX: v.GridX, GY: v.GridY, X: v.X, Y: v.Y, Z: v.Z}
	}
	return DigestBytes("vertex", CanonicalJSON(items))
}

func TriangleDigest(triangles []Triangle) string {
	return DigestBytes("triangle", CanonicalJSON(triangles))
}

func MeshContentHash(projectID string, block Block, mesh *Mesh, deps []Dependency) string {
	payload := struct {
		ProjectID        string       `json:"project_id"`
		Block            Block        `json:"block"`
		Mesh             *Mesh        `json:"mesh"`
		Dependencies     []Dependency `json:"dependencies"`
		AlgorithmVersion string       `json:"algorithm_version"`
	}{
		ProjectID:        projectID,
		Block:            block,
		Mesh:             mesh,
		Dependencies:     deps,
		AlgorithmVersion: AlgorithmVersion,
	}
	return DigestBytes("content", CanonicalJSON(payload))
}

func floatEqual(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	if a == b {
		return true
	}
	return math.Abs(a-b) <= 1e-9*maxFloat(1.0, math.Abs(a), math.Abs(b))
}

func maxFloat(values ...float64) float64 {
	best := values[0]
	for _, v := range values[1:] {
		if v > best {
			best = v
		}
	}
	return best
}

func DiffMesh(blockNo int, expected, actual *Mesh) error {
	if expected == nil || actual == nil {
		idx := 0
		return &DiffError{Location: DiffLocation{BlockNo: blockNo, VertexIndex: &idx, Field: "mesh_nil", Expected: expected != nil, Actual: actual != nil}}
	}
	if len(expected.Vertices) != len(actual.Vertices) {
		idx := min(len(expected.Vertices), len(actual.Vertices))
		return &DiffError{Location: DiffLocation{BlockNo: blockNo, VertexIndex: &idx, Field: "vertices.length", Expected: len(expected.Vertices), Actual: len(actual.Vertices)}}
	}
	for i := range expected.Vertices {
		e, a := expected.Vertices[i], actual.Vertices[i]
		switch {
		case e.GridX != a.GridX:
			return vertexDiff(blockNo, i, "grid_x", e.GridX, a.GridX)
		case e.GridY != a.GridY:
			return vertexDiff(blockNo, i, "grid_y", e.GridY, a.GridY)
		case !floatEqual(e.X, a.X):
			return vertexDiff(blockNo, i, "x", e.X, a.X)
		case !floatEqual(e.Y, a.Y):
			return vertexDiff(blockNo, i, "y", e.Y, a.Y)
		case !floatEqual(e.Z, a.Z):
			return vertexDiff(blockNo, i, "z", e.Z, a.Z)
		}
	}
	if len(expected.Triangles) != len(actual.Triangles) {
		idx := min(len(expected.Triangles), len(actual.Triangles))
		return &DiffError{Location: DiffLocation{BlockNo: blockNo, TriangleIndex: &idx, Field: "triangles.length", Expected: len(expected.Triangles), Actual: len(actual.Triangles)}}
	}
	for i := range expected.Triangles {
		if !reflect.DeepEqual(expected.Triangles[i].Vertices, actual.Triangles[i].Vertices) {
			ti := i
			return &DiffError{Location: DiffLocation{BlockNo: blockNo, TriangleIndex: &ti, Field: "triangle.vertices", Expected: expected.Triangles[i].Vertices, Actual: actual.Triangles[i].Vertices}}
		}
	}
	if len(expected.TriangleErrors) != len(actual.TriangleErrors) {
		ti := min(len(expected.TriangleErrors), len(actual.TriangleErrors))
		return &DiffError{Location: DiffLocation{BlockNo: blockNo, TriangleIndex: &ti, Field: "triangle_errors.length", Expected: len(expected.TriangleErrors), Actual: len(actual.TriangleErrors)}}
	}
	for i := range expected.TriangleErrors {
		e, a := expected.TriangleErrors[i], actual.TriangleErrors[i]
		if e.TriangleIndex != a.TriangleIndex {
			ti := i
			return &DiffError{Location: DiffLocation{BlockNo: blockNo, TriangleIndex: &ti, Field: "triangle_error.index", Expected: e.TriangleIndex, Actual: a.TriangleIndex}}
		}
		if !floatEqual(e.MaxAbsError, a.MaxAbsError) || e.At != a.At {
			ti := i
			return &DiffError{Location: DiffLocation{BlockNo: blockNo, TriangleIndex: &ti, Field: "triangle_error.max_abs_error", Expected: fmt.Sprintf("%+v", e), Actual: fmt.Sprintf("%+v", a)}}
		}
	}
	return nil
}

func vertexDiff(blockNo, index int, field string, expected, actual any) error {
	return &DiffError{Location: DiffLocation{BlockNo: blockNo, VertexIndex: &index, Field: field, Expected: expected, Actual: actual}}
}

func CompareMeshMaps(expected, actual map[int]*Mesh) error {
	for blockNo := 0; blockNo < len(expected); blockNo++ {
		if err := DiffMesh(blockNo, expected[blockNo], actual[blockNo]); err != nil {
			return err
		}
	}
	return nil
}

func CompareBoundaryReports(expected, actual map[string]*BoundaryReport) error {
	if len(expected) != len(actual) {
		return &CodedError{Code: CodeDiff, Path: "boundaries", Message: fmt.Sprintf("boundary count %d != %d", len(expected), len(actual))}
	}
	for id, e := range expected {
		a := actual[id]
		if a == nil {
			return &CodedError{Code: CodeDiff, Path: "boundaries." + id, Message: "missing boundary report"}
		}
		if !floatEqual(e.MaxFoldAngleDeg, a.MaxFoldAngleDeg) || e.Category != a.Category || e.Aligned != a.Aligned {
			return &CodedError{Code: CodeDiff, Path: "boundaries." + id + ".max_fold_angle_deg", Message: fmt.Sprintf("boundary stats differ: expected %.12f/%s, actual %.12f/%s", e.MaxFoldAngleDeg, e.Category, a.MaxFoldAngleDeg, a.Category)}
		}
	}
	return nil
}
