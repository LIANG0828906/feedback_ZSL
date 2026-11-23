package generator

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"meshforge/internal/mesh"
)

const TruthFormatV1 = "meshforge.truth.v1"

type Counts struct {
	Vertices int `json:"vertices"`
	Faces    int `json:"faces"`
	Edges    int `json:"edges"`
	Euler    int `json:"euler_characteristic"`
}

type KnownPoint struct {
	ID       string    `json:"id"`
	Vertex   int       `json:"vertex"`
	Position mesh.Vec3 `json:"position"`
	Observed mesh.Vec3 `json:"observed"`
}

type HoleTruth struct {
	ID        string      `json:"id"`
	VertexIDs []int       `json:"vertex_ids"`
	Points    []mesh.Vec3 `json:"points"`
}

type DiagnosticTruth struct {
	NonManifoldEdges         int `json:"non_manifold_edges"`
	DuplicateVertices        int `json:"duplicate_vertices"`
	FlippedFaces             int `json:"flipped_faces"`
	OrientationConflictEdges int `json:"orientation_conflict_edges"`
	BoundaryLoops            int `json:"boundary_loops"`
	OpenBoundaryChains       int `json:"open_boundary_chains"`
	Components               int `json:"components"`
	ThinSheetComponents      int `json:"thin_sheet_components"`
	SelfIntersections        int `json:"self_intersections"`
}

type Truth struct {
	Format            string          `json:"format"`
	ID                string          `json:"id"`
	Seed              int64           `json:"seed"`
	Level             int             `json:"level"`
	InputVertices     int             `json:"input_vertices"`
	InputFaces        int             `json:"input_faces"`
	Original          Counts          `json:"original"`
	Repaired          Counts          `json:"repaired"`
	KnownPoints       []KnownPoint    `json:"known_points"`
	HoleBoundaryLoops []HoleTruth     `json:"hole_boundary_loops"`
	Defects           DiagnosticTruth `json:"defects"`
	FlippedFaceIDs    []int           `json:"flipped_face_ids"`
	HoleFaceIDs       []int           `json:"hole_face_ids"`
	ThinSheetFaces    []int           `json:"thin_sheet_faces"`
	NonManifoldFace   int             `json:"non_manifold_face"`
}

type Sample struct {
	Mesh  mesh.Mesh
	Truth Truth
}

type rng uint64

func newRNG(seed int64) *rng {
	v := uint64(seed) ^ 0x9e3779b97f4a7c15
	r := rng(v)
	_ = r.next()
	return &r
}

func (r *rng) next() uint64 {
	x := uint64(*r)
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	*r = rng(x)
	return x * 2685821657736338717
}

func (r *rng) intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func Baseline(level int) mesh.Mesh {
	verts := []mesh.Vec3{
		{X: 1}, {X: -1}, {Y: 1}, {Y: -1}, {Z: 1}, {Z: -1},
	}
	faces := []mesh.Face{
		{A: 4, B: 0, C: 2}, {A: 4, B: 2, C: 1},
		{A: 4, B: 1, C: 3}, {A: 4, B: 3, C: 0},
		{A: 5, B: 2, C: 0}, {A: 5, B: 1, C: 2},
		{A: 5, B: 3, C: 1}, {A: 5, B: 0, C: 3},
	}
	for i := range verts {
		verts[i] = verts[i].Normalize()
	}
	m := mesh.Mesh{Vertices: verts, Faces: faces}
	for l := 0; l < level; l++ {
		m = subdivide(m)
	}
	return m
}

func subdivide(m mesh.Mesh) mesh.Mesh {
	out := mesh.Mesh{Vertices: append([]mesh.Vec3(nil), m.Vertices...)}
	mid := make(map[mesh.EdgeKey]int, len(m.Faces)*3/2)
	makeMid := func(a, b int) int {
		k := mesh.MakeEdge(a, b)
		if idx, ok := mid[k]; ok {
			return idx
		}
		v := m.Vertices[a].Add(m.Vertices[b]).Mul(0.5).Normalize()
		idx := len(out.Vertices)
		out.Vertices = append(out.Vertices, v)
		mid[k] = idx
		return idx
	}
	for _, f := range m.Faces {
		ab := makeMid(f.A, f.B)
		bc := makeMid(f.B, f.C)
		ca := makeMid(f.C, f.A)
		out.Faces = append(out.Faces,
			mesh.Face{A: f.A, B: ab, C: ca},
			mesh.Face{A: ab, B: f.B, C: bc},
			mesh.Face{A: ca, B: bc, C: f.C},
			mesh.Face{A: ab, B: bc, C: ca},
		)
	}
	return out
}

func Generate(level int, seed int64, id string) (Sample, error) {
	if level < 2 || level > 9 {
		return Sample{}, fmt.Errorf("level must be in [2,9]")
	}
	base := Baseline(level)
	r := newRNG(seed + int64(level)*131)
	vertices := append([]mesh.Vec3(nil), base.Vertices...)
	faces := append([]mesh.Face(nil), base.Faces...)
	origCounts := counts(base)

	// Radial noise is deliberately high frequency so a volume-preserving
	// Laplacian pass measurably reduces the error at known points.
	noisy := make([]int, 0)
	noiseCount := max(8, len(vertices)/40)
	noiseSet := make(map[int]bool)
	for len(noisy) < noiseCount {
		vi := 7 + r.intn(len(vertices)-14)
		if !noiseSet[vi] {
			noiseSet[vi] = true
			noisy = append(noisy, vi)
		}
	}
	sort.Ints(noisy)
	for _, vi := range noisy {
		n := vertices[vi].Normalize()
		amp := 0.018 + float64(r.intn(1000))*0.000025
		sign := 1.0
		if r.next()&1 == 0 {
			sign = -1
		}
		vertices[vi] = vertices[vi].Add(n.Mul(sign * amp))
	}

	// Two disjoint triangular holes create two boundary loops.
	holeFaces := make([]int, 0, 2)
	usedHoleVerts := make(map[int]bool)
	for attempt := 0; attempt < 2000 && len(holeFaces) < 2; attempt++ {
		fi := r.intn(len(faces))
		f := faces[fi]
		ok := true
		for _, vi := range f.Indices() {
			if usedHoleVerts[vi] {
				ok = false
			}
		}
		if ok {
			holeFaces = append(holeFaces, fi)
			for _, vi := range f.Indices() {
				usedHoleVerts[vi] = true
			}
		}
	}
	sort.Ints(holeFaces)
	holeTruth := make([]HoleTruth, 0, len(holeFaces))
	for i, fi := range holeFaces {
		f := faces[fi]
		ht := HoleTruth{ID: fmt.Sprintf("H%d", i+1), VertexIDs: []int{f.A, f.B, f.C}}
		for _, vi := range ht.VertexIDs {
			ht.Points = append(ht.Points, vertices[vi])
		}
		holeTruth = append(holeTruth, ht)
	}
	removeFaceSet := make(map[int]bool, len(holeFaces))
	for _, fi := range holeFaces {
		removeFaceSet[fi] = true
	}

	flippedIDs := make([]int, 0, 5)
	flipVertexUse := make(map[int]bool)
	for attempt := 0; attempt < 2000 && len(flippedIDs) < 5; attempt++ {
		fi := r.intn(len(faces))
		if removeFaceSet[fi] {
			continue
		}
		f := faces[fi]
		ok := true
		for _, vi := range f.Indices() {
			if flipVertexUse[vi] {
				ok = false
			}
		}
		if ok {
			flippedIDs = append(flippedIDs, fi)
			for _, vi := range f.Indices() {
				flipVertexUse[vi] = true
			}
		}
	}
	sort.Ints(flippedIDs)
	for _, fi := range flippedIDs {
		faces[fi] = faces[fi].Reversed()
	}

	var keptFaces []mesh.Face
	oldFaceToNew := make(map[int]int, len(faces))
	for i, f := range faces {
		if !removeFaceSet[i] {
			oldFaceToNew[i] = len(keptFaces)
			keptFaces = append(keptFaces, f)
		}
	}
	faces = keptFaces
	flippedFinal := make([]int, 0, len(flippedIDs))
	for _, old := range flippedIDs {
		if fi, ok := oldFaceToNew[old]; ok {
			flippedFinal = append(flippedFinal, fi)
		}
	}
	holeHighlightFaces := holeNeighborFaces(mesh.Mesh{Vertices: vertices, Faces: faces}, holeTruth)

	// Duplicate vertices are exact copies; selected incident corners are
	// remapped so welding is observable rather than a no-op.
	dupCount := min(48, max(12, len(vertices)/180))
	dupSet := make(map[int]bool)
	var duplicates []int
	for attempt := 0; attempt < 2000 && len(duplicates) < dupCount; attempt++ {
		vi := 6 + r.intn(len(vertices)-12)
		if !dupSet[vi] {
			dupSet[vi] = true
			duplicates = append(duplicates, vi)
		}
	}
	sort.Ints(duplicates)
	dupMap := make(map[int]int, len(duplicates))
	for _, vi := range duplicates {
		dupMap[vi] = len(vertices)
		vertices = append(vertices, vertices[vi])
	}
	incident := make([]int, len(vertices))
	for fi := range faces {
		for _, vi := range faces[fi].Indices() {
			incident[vi]++
		}
	}
	remapSeen := make(map[int]bool)
	for fi := range faces {
		idx := faces[fi].Indices()
		for k, vi := range idx {
			if dup, ok := dupMap[vi]; ok {
				incident[vi]--
				if incident[vi]%2 == 0 && !remapSeen[vi] {
					remapSeen[vi] = true
					idx[k] = dup
				}
			}
		}
		faces[fi] = mesh.Face{A: idx[0], B: idx[1], C: idx[2]}
	}

	// A single attached flap creates exactly one non-manifold edge.
	nmFace, ok := pickManifoldEdgeFace(faces, r)
	if !ok {
		return Sample{}, fmt.Errorf("failed to choose non-manifold edge")
	}
	nmf := faces[nmFace]
	u, v := nmf.A, nmf.B
	apex := vertices[u].Add(vertices[v]).Mul(0.5).Normalize().Mul(1.36)
	apexIdx := len(vertices)
	vertices = append(vertices, apex)
	faces = append(faces, mesh.Face{A: u, B: apexIdx, C: v})

	// A flat, open quad pierces the sphere. It is a separate component and
	// produces measurable triangle-pair intersections.
	sheetStart := len(vertices)
	sheetVerts := []mesh.Vec3{
		{X: 0.06, Y: 0.013, Z: 0.08},
		{X: 0.32, Y: 0.013, Z: 0.08},
		{X: 0.06, Y: 0.013, Z: 1.42},
		{X: 0.32, Y: 0.013, Z: 1.42},
	}
	vertices = append(vertices, sheetVerts...)
	thinFaces := []int{len(faces), len(faces) + 1}
	faces = append(faces,
		mesh.Face{A: sheetStart, B: sheetStart + 1, C: sheetStart + 3},
		mesh.Face{A: sheetStart, B: sheetStart + 3, C: sheetStart + 2},
	)

	sampleMesh := mesh.Mesh{Vertices: vertices, Faces: faces}
	if err := validateIndices(sampleMesh); err != nil {
		return Sample{}, err
	}
	stats := DiagnoseCounts(sampleMesh)
	boundary := mesh.BoundaryLoops(sampleMesh)
	orientation := mesh.AnalyzeOrientation(sampleMesh)
	stats.BoundaryLoops = len(boundary.Loops)
	stats.OpenBoundaryChains = boundary.OpenChains
	stats.FlippedFaces = orientation.FlippedFaces
	stats.OrientationConflictEdges = orientation.ConflictEdges

	known := make([]KnownPoint, 0, 8)
	for i := 0; i < len(noisy) && len(known) < 8; i++ {
		vi := noisy[i]
		known = append(known, KnownPoint{
			ID:       fmt.Sprintf("P%d", len(known)+1),
			Vertex:   vi,
			Position: base.Vertices[vi],
			Observed: sampleMesh.Vertices[vi],
		})
	}
	truth := Truth{
		Format:            TruthFormatV1,
		ID:                id,
		Seed:              seed,
		Level:             level,
		InputVertices:     len(sampleMesh.Vertices),
		InputFaces:        len(sampleMesh.Faces),
		Original:          origCounts,
		Repaired:          origCounts,
		KnownPoints:       known,
		HoleBoundaryLoops: holeTruth,
		Defects:           stats,
		FlippedFaceIDs:    flippedFinal,
		HoleFaceIDs:       holeHighlightFaces,
		ThinSheetFaces:    thinFaces,
		NonManifoldFace:   nmFace,
	}
	return Sample{Mesh: sampleMesh, Truth: truth}, nil
}

func holeNeighborFaces(m mesh.Mesh, holes []HoleTruth) []int {
	topo := mesh.BuildTopology(m)
	seen := make(map[int]bool)
	var out []int
	for _, hole := range holes {
		verts := make(map[int]bool, len(hole.VertexIDs))
		for _, vi := range hole.VertexIDs {
			verts[vi] = true
		}
		for _, e := range topo.SortedEdges() {
			if len(e.Uses) != 1 || !verts[e.A] || !verts[e.B] {
				continue
			}
			fi := e.Uses[0].Face
			if !seen[fi] {
				seen[fi] = true
				out = append(out, fi)
			}
		}
	}
	sort.Ints(out)
	return out
}

func DiagnoseCounts(m mesh.Mesh) DiagnosticTruth {
	t := mesh.BuildTopology(m)
	var nm int
	for _, e := range t.Edges {
		if len(e.Uses) > 2 {
			nm++
		}
	}
	comps := mesh.FaceComponents(m, mesh.ComponentAllSharedEdges)
	return DiagnosticTruth{
		NonManifoldEdges:    nm,
		DuplicateVertices:   mesh.DuplicateVertexCount(m),
		Components:          len(comps),
		ThinSheetComponents: mesh.ThinSheetComponentCount(m),
		SelfIntersections:   mesh.CountSelfIntersections(m),
	}
}

func counts(m mesh.Mesh) Counts {
	edges := len(mesh.BuildTopology(m).Edges)
	return Counts{
		Vertices: len(m.Vertices),
		Faces:    len(m.Faces),
		Edges:    edges,
		Euler:    len(m.Vertices) - edges + len(m.Faces),
	}
}

func pickManifoldEdgeFace(faces []mesh.Face, r *rng) (int, bool) {
	type edgeCount struct {
		face int
		a, b int
	}
	counts := make(map[mesh.EdgeKey]int)
	for _, f := range faces {
		for _, e := range [][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}} {
			counts[mesh.MakeEdge(e[0], e[1])]++
		}
	}
	var candidates []edgeCount
	for fi, f := range faces {
		for _, e := range [][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}} {
			if counts[mesh.MakeEdge(e[0], e[1])] == 2 {
				candidates = append(candidates, edgeCount{face: fi, a: e[0], b: e[1]})
				break
			}
		}
	}
	if len(candidates) == 0 {
		return 0, false
	}
	return candidates[r.intn(len(candidates))].face, true
}

func validateIndices(m mesh.Mesh) error {
	for fi, f := range m.Faces {
		for _, vi := range f.Indices() {
			if vi < 0 || vi >= len(m.Vertices) {
				return fmt.Errorf("face %d index out of range", fi)
			}
		}
	}
	return nil
}

func SaveSample(dir string, s Sample) error {
	if err := mesh.SaveFile(filepath.Join(dir, "mesh.json"), s.Mesh); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.Truth, "", "  ")
	if err != nil {
		return err
	}
	return mesh.WriteFileAtomic(filepath.Join(dir, "truth.json"), append(data, '\n'), 0o644)
}

func LoadSample(dir string) (Sample, error) {
	m, err := mesh.LoadFile(filepath.Join(dir, "mesh.json"))
	if err != nil {
		return Sample{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return Sample{}, err
	}
	var t Truth
	if err := json.Unmarshal(data, &t); err != nil {
		return Sample{}, err
	}
	if t.Format != TruthFormatV1 {
		return Sample{}, fmt.Errorf("unsupported truth format %q", t.Format)
	}
	return Sample{Mesh: m, Truth: t}, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func CloseEnough(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}
