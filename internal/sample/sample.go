package sample

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
)

const DefaultSeed int64 = 424242

type KnownPoint struct {
	ID        string    `json:"id"`
	Position  mesh.Vec3 `json:"position"`
	Tolerance float64   `json:"tolerance"`
	Note      string    `json:"note,omitempty"`
}

type HoleRing struct {
	ID          string      `json:"id"`
	Coordinates []mesh.Vec3 `json:"coordinates"`
	Tolerance   float64     `json:"tolerance"`
}

type Defect struct {
	Kind        string      `json:"kind"`
	Description string      `json:"description"`
	Faces       []int       `json:"faces,omitempty"`
	Coordinates []mesh.Vec3 `json:"coordinates,omitempty"`
}

type Truth struct {
	Primitive string `json:"primitive"`
	Vertices  int    `json:"vertices"`
	Faces     int    `json:"faces"`
	Edges     int    `json:"edges"`
	Euler     int    `json:"euler"`
}

type Manifest struct {
	Schema        string       `json:"schema"`
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Seed          int64        `json:"seed"`
	InputVertices int          `json:"input_vertices"`
	InputFaces    int          `json:"input_faces"`
	Truth         Truth        `json:"truth"`
	KnownPoints   []KnownPoint `json:"known_points"`
	HoleRings     []HoleRing   `json:"hole_rings"`
	Defects       []Defect     `json:"defects"`
}

type Sample struct {
	Manifest Manifest  `json:"manifest"`
	Mesh     mesh.Mesh `json:"mesh"`
}

func GenerateAll(seed int64) ([]Sample, error) {
	if seed == 0 {
		seed = DefaultSeed
	}
	rng := rand.New(rand.NewSource(seed))
	builders := []func(*rand.Rand, int64) Sample{buildTetra, buildCube, buildOcta}
	out := make([]Sample, 0, len(builders))
	for i, build := range builders {
		s := build(rng, seed+int64(i)*1009)
		s.Manifest.Seed = seed
		if err := ValidateLoadedMesh(&s.Mesh, &s.Manifest); err != nil {
			return nil, fmt.Errorf("generated sample %s invalid: %w", s.Manifest.ID, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func WriteWorkspace(dir string, seed int64) ([]Sample, error) {
	samples, err := GenerateAll(seed)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"samples", "truth"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	index := make([]Manifest, 0, len(samples))
	for _, s := range samples {
		if err := writeJSON(filepath.Join(dir, "samples", s.Manifest.ID+".input.json"), s.Mesh); err != nil {
			return nil, err
		}
		if err := writeJSON(filepath.Join(dir, "truth", s.Manifest.ID+".manifest.json"), s.Manifest); err != nil {
			return nil, err
		}
		index = append(index, s.Manifest)
	}
	if err := writeJSON(filepath.Join(dir, "truth", "manifests.json"), index); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "workspace_manifest.json"), map[string]any{
		"schema":       "meshdoctor-workspace-v1",
		"seed":         seed,
		"sample_count": len(samples),
		"samples":      index,
	}); err != nil {
		return nil, err
	}
	return samples, nil
}

func LoadSample(dir, id string) (*Sample, error) {
	var m mesh.Mesh
	if err := readJSON(filepath.Join(dir, "samples", id+".input.json"), &m); err != nil {
		return nil, err
	}
	var man Manifest
	if err := readJSON(filepath.Join(dir, "truth", id+".manifest.json"), &man); err != nil {
		return nil, err
	}
	if err := ValidateLoadedMesh(&m, &man); err != nil {
		return nil, err
	}
	return &Sample{Manifest: man, Mesh: m}, nil
}

func LoadAll(dir string) ([]Sample, error) {
	var index []Manifest
	if err := readJSON(filepath.Join(dir, "truth", "manifests.json"), &index); err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(index))
	for _, meta := range index {
		s, err := LoadSample(dir, meta.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, nil
}

func ValidateLoadedMesh(m *mesh.Mesh, man *Manifest) error {
	if err := m.ValidateIndices(); err != nil {
		return err
	}
	if bad := m.DegenerateFaces(1e-10); len(bad) > 0 {
		err := errs.New(errs.EMETA, "degenerate face at index %d", bad[0])
		return errs.WithDetails(err, map[string]any{"field": "faces", "face_index": bad[0]})
	}
	if man.InputVertices >= 0 && len(m.Vertices) != man.InputVertices {
		err := errs.New(errs.EMETA, "field input_vertices mismatch: manifest=%d actual=%d", man.InputVertices, len(m.Vertices))
		return errs.WithDetails(err, map[string]any{
			"field": "input_vertices", "expected": man.InputVertices, "actual": len(m.Vertices),
		})
	}
	if man.InputFaces >= 0 && len(m.Faces) != man.InputFaces {
		err := errs.New(errs.EMETA, "field input_faces mismatch: manifest=%d actual=%d", man.InputFaces, len(m.Faces))
		return errs.WithDetails(err, map[string]any{
			"field": "input_faces", "expected": man.InputFaces, "actual": len(m.Faces),
		})
	}
	truth := man.Truth
	if truth.Vertices > 0 && truth.Faces > 0 {
		expectedEdges := truth.Vertices + truth.Faces - truth.Euler
		if truth.Edges != expectedEdges {
			err := errs.New(errs.EMETA, "field truth.edges inconsistent with euler: edges=%d expected=%d", truth.Edges, expectedEdges)
			return errs.WithDetail(err, "field", "truth.edges")
		}
		if truth.Euler != truth.Vertices-truth.Edges+truth.Faces {
			err := errs.New(errs.EMETA, "field truth.euler mismatch: declared=%d calculated=%d", truth.Euler, truth.Vertices-truth.Edges+truth.Faces)
			return errs.WithDetail(err, "field", "truth.euler")
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func cloneVertices(in []mesh.Vec3) []mesh.Vec3 { return append([]mesh.Vec3(nil), in...) }

func ringCoordinates(vertices []mesh.Vec3, indices []int) []mesh.Vec3 {
	out := make([]mesh.Vec3, len(indices))
	for i, idx := range indices {
		out[i] = vertices[idx]
	}
	return out
}

func buildTetra(rng *rand.Rand, seed int64) Sample {
	verts := []mesh.Vec3{
		{1, 1, 1}, {1, -1, -1}, {-1, 1, -1}, {-1, -1, 1},
	}
	baseFaces := []mesh.Face{
		{A: 0, B: 1, C: 2}, {A: 0, B: 3, C: 1}, {A: 0, B: 2, C: 3}, {A: 1, B: 3, C: 2},
	}
	holeIndices := []int{0, 1, 2}
	holeCoords := ringCoordinates(verts, holeIndices)

	faces := []mesh.Face{baseFaces[1], baseFaces[2], baseFaces[3]}
	dupIndex := len(verts)
	verts = append(verts, verts[3])
	faces[0] = mesh.Face{A: 0, B: dupIndex, C: 1}
	faces[1] = mesh.Face{A: 0, B: 3, C: 2}
	faces = append(faces, baseFaces[3])

	man := Manifest{
		ID: "tetra", Name: "开裂四面体（重复顶点/翻转面/非流形重复面/小孔）", Seed: seed,
		Truth:       Truth{Primitive: "tetrahedron", Vertices: 4, Faces: 4, Edges: 6, Euler: 2},
		KnownPoints: []KnownPoint{{ID: "kp-t-0", Position: verts[0], Tolerance: 1e-7, Note: "孔环接缝角点，平滑时固定"}, {ID: "kp-t-1", Position: verts[1], Tolerance: 1e-7, Note: "孔环接缝角点，平滑时固定"}},
		HoleRings:   []HoleRing{{ID: "hole-t-0", Coordinates: holeCoords, Tolerance: 1e-8}},
		Defects: []Defect{
			{Kind: "duplicate_vertex", Description: "顶点 4 是顶点 3 的精确副本，一个面改引副本", Faces: []int{0}, Coordinates: []mesh.Vec3{verts[3]}},
			{Kind: "flipped_face", Description: "面 1 索引顺序被反转", Faces: []int{1}},
			{Kind: "non_manifold_duplicate_face", Description: "末尾重复面造成边引用数大于 2", Faces: []int{3}},
			{Kind: "small_hole", Description: "移除原始三角面 0", Coordinates: holeCoords},
		},
	}
	man.InputVertices = len(verts)
	man.InputFaces = len(faces)
	return Sample{Manifest: man, Mesh: mesh.Mesh{Vertices: verts, Faces: faces}}
}

func buildCube(rng *rand.Rand, seed int64) Sample {
	verts := []mesh.Vec3{
		{-0.5, -0.5, -0.5}, {0.5, -0.5, -0.5}, {0.5, 0.5, -0.5}, {-0.5, 0.5, -0.5},
		{-0.5, -0.5, 0.5}, {0.5, -0.5, 0.5}, {0.5, 0.5, 0.5}, {-0.5, 0.5, 0.5},
	}
	quads := [][4]int{
		{0, 3, 2, 1}, {4, 5, 6, 7}, {0, 1, 5, 4},
		{3, 7, 6, 2}, {0, 4, 7, 3}, {1, 2, 6, 5},
	}
	var base []mesh.Face
	for _, q := range quads {
		base = append(base, mesh.Face{A: q[0], B: q[1], C: q[2]}, mesh.Face{A: q[0], B: q[2], C: q[3]})
	}
	holeIndices := []int{4, 5, 6, 7}
	holeCoords := ringCoordinates(verts, holeIndices)

	var faces []mesh.Face
	for i, f := range base {
		if i == 2 || i == 3 {
			continue
		}
		faces = append(faces, f)
	}
	dup := len(verts)
	verts = append(verts, verts[0])
	faces[0] = mesh.Face{A: dup, B: 3, C: 2}
	for i := range faces {
		if faces[i] == (mesh.Face{A: 3, B: 7, C: 6}) {
			faces[i] = mesh.Face{A: 3, C: 7, B: 6}
			break
		}
	}
	faces = append(faces, mesh.Face{A: 1, B: 2, C: 6})

	verts[2] = mesh.Vec3{0.02, 0.02, 0.43}

	man := Manifest{
		ID: "cube", Name: "自相交薄片立方体（四边形孔/重复顶点/翻转/非流形）", Seed: seed,
		Truth: Truth{Primitive: "triangulated-cube", Vertices: 8, Faces: 12, Edges: 18, Euler: 2},
		KnownPoints: []KnownPoint{
			{ID: "kp-c-4", Position: mesh.Vec3{-0.5, -0.5, 0.5}, Tolerance: 1e-7, Note: "孔环接缝角点，平滑时固定"},
			{ID: "kp-c-1", Position: mesh.Vec3{0.5, -0.5, -0.5}, Tolerance: 0.8},
			{ID: "kp-c-center-front", Position: mesh.Vec3{0, -0.5, 0}, Tolerance: 0.8, Note: "前面解析中心点，允许拉普拉斯收缩"},
		},
		HoleRings: []HoleRing{{ID: "hole-c-top", Coordinates: holeCoords, Tolerance: 1e-8}},
		Defects: []Defect{
			{Kind: "quad_hole", Description: "移除顶面两个三角面，形成四边形边界环", Coordinates: holeCoords},
			{Kind: "duplicate_vertex", Description: "顶点 8 是顶点 0 的精确副本", Faces: []int{0}, Coordinates: []mesh.Vec3{verts[8]}},
			{Kind: "flipped_face", Description: "输入面 4（背面）被翻转", Faces: []int{4}},
			{Kind: "non_manifold_duplicate_face", Description: "输入面 10 是重复右面三角面", Faces: []int{10}, Coordinates: []mesh.Vec3{verts[1], verts[2], verts[6]}},
			{Kind: "self_intersecting_sliver", Description: "顶点 2 被推入顶面附近，形成细长相交薄片", Coordinates: []mesh.Vec3{verts[2]}},
		},
	}
	man.InputVertices = len(verts)
	man.InputFaces = len(faces)
	return Sample{Manifest: man, Mesh: mesh.Mesh{Vertices: verts, Faces: faces}}
}

func buildOcta(rng *rand.Rand, seed int64) Sample {
	verts := []mesh.Vec3{
		{1, 0, 0}, {-1, 0, 0}, {0, 1, 0}, {0, -1, 0}, {0, 0, 1}, {0, 0, -1},
	}
	baseFaces := []mesh.Face{
		{A: 0, B: 2, C: 4}, {A: 2, B: 1, C: 4}, {A: 1, B: 3, C: 4}, {A: 3, B: 0, C: 4},
		{A: 2, B: 0, C: 5}, {A: 1, B: 2, C: 5}, {A: 3, B: 1, C: 5}, {A: 0, B: 3, C: 5},
	}
	holeIndices := []int{0, 2, 4}
	holeCoords := ringCoordinates(verts, holeIndices)

	faces := append([]mesh.Face(nil), baseFaces[1:]...)
	dup := len(verts)
	verts = append(verts, verts[5])
	for i := range faces {
		if faces[i] == (mesh.Face{A: 3, B: 1, C: 5}) {
			faces[i] = mesh.Face{A: 3, B: 1, C: dup}
			break
		}
	}
	faces[0] = mesh.Face{A: 2, C: 1, B: 4}
	faces = append(faces, mesh.Face{A: 2, B: 0, C: 5})
	verts[3] = mesh.Vec3{0.08, 0.72, 0.08}

	man := Manifest{
		ID: "octa", Name: "细长自相交八面体（三角孔/重复顶点/翻转/非流形）", Seed: seed,
		Truth: Truth{Primitive: "octahedron", Vertices: 6, Faces: 8, Edges: 12, Euler: 2},
		KnownPoints: []KnownPoint{
			{ID: "kp-o-1", Position: mesh.Vec3{-1, 0, 0}, Tolerance: 1.1},
			{ID: "kp-o-5", Position: mesh.Vec3{0, 0, -1}, Tolerance: 1.1},
			{ID: "kp-o-south", Position: mesh.Vec3{0, -0.333333333333, -0.333333333333}, Tolerance: 1.1},
		},
		HoleRings: []HoleRing{{ID: "hole-o-0", Coordinates: holeCoords, Tolerance: 1e-8}},
		Defects: []Defect{
			{Kind: "small_hole", Description: "移除原始三角面 {0,2,4}", Coordinates: holeCoords},
			{Kind: "duplicate_vertex", Description: "顶点 6 是南极点 5 的副本，输入面 5 改引副本", Faces: []int{5}},
			{Kind: "flipped_face", Description: "输入面 0 被反向", Faces: []int{0}},
			{Kind: "non_manifold_duplicate_face", Description: "输入面 7 重复底面 {2,0,5}", Faces: []int{7}},
			{Kind: "self_intersecting_sliver", Description: "赤道顶点 3 被推过中心，形成自相交薄片", Coordinates: []mesh.Vec3{verts[3]}},
		},
	}
	man.InputVertices = len(verts)
	man.InputFaces = len(faces)
	return Sample{Manifest: man, Mesh: mesh.Mesh{Vertices: verts, Faces: faces}}
}
