package mesh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (v Vec3) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("[%s,%s,%s]",
		number(v.X), number(v.Y), number(v.Z))), nil
}

func number(v float64) string {
	return fmt.Sprintf("%.17g", v)
}

func (v *Vec3) UnmarshalJSON(data []byte) error {
	var a []float64
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	if len(a) != 3 {
		return fmt.Errorf("vertex must contain exactly 3 numbers")
	}
	v.X, v.Y, v.Z = a[0], a[1], a[2]
	return nil
}

func (v Vec3) Add(o Vec3) Vec3    { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3    { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Mul(s float64) Vec3 { return Vec3{v.X * s, v.Y * s, v.Z * s} }
func (v Vec3) Dot(o Vec3) float64 { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }
func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}
func (v Vec3) Len() float64   { return math.Sqrt(v.Dot(v)) }
func (v Vec3) LenSq() float64 { return v.Dot(v) }
func (v Vec3) Normalize() Vec3 {
	l := v.Len()
	if l == 0 {
		return Vec3{}
	}
	return v.Mul(1 / l)
}
func (v Vec3) Distance(o Vec3) float64 { return v.Sub(o).Len() }

type Face struct {
	A int `json:"a"`
	B int `json:"b"`
	C int `json:"c"`
}

func (f Face) Vertex(i int) int {
	switch i {
	case 0:
		return f.A
	case 1:
		return f.B
	default:
		return f.C
	}
}

func (f Face) Indices() [3]int { return [3]int{f.A, f.B, f.C} }
func (f Face) Reversed() Face  { return Face{A: f.A, B: f.C, C: f.B} }

type Mesh struct {
	Vertices []Vec3 `json:"vertices"`
	Faces    []Face `json:"faces"`
}

func (m Mesh) Clone() Mesh {
	out := Mesh{
		Vertices: append([]Vec3(nil), m.Vertices...),
		Faces:    append([]Face(nil), m.Faces...),
	}
	return out
}

func (m Mesh) Bounds() (min, max Vec3) {
	if len(m.Vertices) == 0 {
		return Vec3{}, Vec3{}
	}
	min, max = m.Vertices[0], m.Vertices[0]
	for _, v := range m.Vertices[1:] {
		if v.X < min.X {
			min.X = v.X
		}
		if v.Y < min.Y {
			min.Y = v.Y
		}
		if v.Z < min.Z {
			min.Z = v.Z
		}
		if v.X > max.X {
			max.X = v.X
		}
		if v.Y > max.Y {
			max.Y = v.Y
		}
		if v.Z > max.Z {
			max.Z = v.Z
		}
	}
	return min, max
}

func (m Mesh) FaceNormal(i int) Vec3 {
	f := m.Faces[i]
	a, b, c := m.Vertices[f.A], m.Vertices[f.B], m.Vertices[f.C]
	return b.Sub(a).Cross(c.Sub(a)).Normalize()
}

func (m Mesh) FaceArea(i int) float64 {
	f := m.Faces[i]
	a, b, c := m.Vertices[f.A], m.Vertices[f.B], m.Vertices[f.C]
	return b.Sub(a).Cross(c.Sub(a)).Len() * 0.5
}

func (m Mesh) SurfaceArea() float64 {
	var area float64
	for i := range m.Faces {
		area += m.FaceArea(i)
	}
	return area
}

func (m Mesh) SignedVolume() float64 {
	var volume float64
	for _, f := range m.Faces {
		a, b, c := m.Vertices[f.A], m.Vertices[f.B], m.Vertices[f.C]
		volume += a.Dot(b.Cross(c)) / 6.0
	}
	return volume
}

// ContentHash is canonical and independent of JSON field ordering or map
// iteration. It intentionally includes only geometry, not provenance.
func (m Mesh) ContentHash() string {
	h := sha256.New()
	var buf [8]byte
	writeU64 := func(v uint64) {
		for i := 0; i < 8; i++ {
			buf[i] = byte(v >> (8 * i))
		}
		_, _ = h.Write(buf[:])
	}
	writeU64(uint64(len(m.Vertices)))
	for _, v := range m.Vertices {
		writeU64(math.Float64bits(v.X))
		writeU64(math.Float64bits(v.Y))
		writeU64(math.Float64bits(v.Z))
	}
	writeU64(uint64(len(m.Faces)))
	for _, f := range m.Faces {
		writeU64(uint64(uint32(f.A)))
		writeU64(uint64(uint32(f.B)))
		writeU64(uint64(uint32(f.C)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type EdgeKey struct {
	A int `json:"a"`
	B int `json:"b"`
}

func MakeEdge(a, b int) EdgeKey {
	if a > b {
		a, b = b, a
	}
	return EdgeKey{A: a, B: b}
}

type EdgeUse struct {
	Face int `json:"face"`
	From int `json:"from"`
	To   int `json:"to"`
}

type EdgeRecord struct {
	A    int       `json:"a"`
	B    int       `json:"b"`
	Uses []EdgeUse `json:"uses"`
}

type Topology struct {
	Edges      map[EdgeKey]*EdgeRecord
	VertexUses []int
}

func BuildTopology(m Mesh) Topology {
	t := Topology{
		Edges:      make(map[EdgeKey]*EdgeRecord, len(m.Faces)*3/2),
		VertexUses: make([]int, len(m.Vertices)),
	}
	for fi, f := range m.Faces {
		for _, idx := range []int{f.A, f.B, f.C} {
			if idx >= 0 && idx < len(t.VertexUses) {
				t.VertexUses[idx]++
			}
		}
		edges := [3][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}}
		for _, e := range edges {
			k := MakeEdge(e[0], e[1])
			r := t.Edges[k]
			if r == nil {
				r = &EdgeRecord{A: k.A, B: k.B}
				t.Edges[k] = r
			}
			r.Uses = append(r.Uses, EdgeUse{Face: fi, From: e[0], To: e[1]})
		}
	}
	return t
}

func (t Topology) SortedEdges() []*EdgeRecord {
	out := make([]*EdgeRecord, 0, len(t.Edges))
	for _, e := range t.Edges {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}

type Stats struct {
	Vertices                 int     `json:"vertices"`
	Faces                    int     `json:"faces"`
	Edges                    int     `json:"edges"`
	EulerCharacteristic      int     `json:"euler_characteristic"`
	NonManifoldEdges         int     `json:"non_manifold_edges"`
	DuplicateVertices        int     `json:"duplicate_vertices"`
	FlippedFaces             int     `json:"flipped_faces"`
	OrientationConflictEdges int     `json:"orientation_conflict_edges"`
	BoundaryLoops            int     `json:"boundary_loops"`
	OpenBoundaryChains       int     `json:"open_boundary_chains"`
	Components               int     `json:"components"`
	ThinSheetComponents      int     `json:"thin_sheet_components"`
	SelfIntersections        int     `json:"self_intersections"`
	UnusedVertices           int     `json:"unused_vertices"`
	MinArea                  float64 `json:"min_area"`
	TotalArea                float64 `json:"total_area"`
	SignedVolume             float64 `json:"signed_volume"`
}

func (s Stats) TopologyEqual(o Stats) bool {
	return s.Vertices == o.Vertices && s.Faces == o.Faces &&
		s.Edges == o.Edges && s.EulerCharacteristic == o.EulerCharacteristic
}
