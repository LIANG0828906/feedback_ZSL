package repair

import (
	"fmt"
	"math"
	"sort"

	"meshforge/internal/errs"
	"meshforge/internal/generator"
	"meshforge/internal/mesh"
)

type WeldOptions struct {
	Tolerance       float64
	MinComponentPct float64
}

type WeldReport struct {
	Tolerance               float64 `json:"tolerance"`
	SuggestedTolerance      float64 `json:"suggested_tolerance"`
	InputVertices           int     `json:"input_vertices"`
	OutputVertices          int     `json:"output_vertices"`
	MergedVertices          int     `json:"merged_vertices"`
	DuplicateFacesRemoved   int     `json:"duplicate_faces_removed"`
	NonManifoldFacesRemoved int     `json:"non_manifold_faces_removed"`
	ComponentsRemoved       int     `json:"components_removed"`
	ComponentFacesRemoved   int     `json:"component_faces_removed"`
	FinalCompactVertices    int     `json:"final_compact_vertices"`
}

type unionFind struct {
	parent []int
	rank   []uint8
}

func newUnionFind(n int) *unionFind {
	u := &unionFind{parent: make([]int, n), rank: make([]uint8, n)}
	for i := range u.parent {
		u.parent[i] = i
	}
	return u
}

func (u *unionFind) find(x int) int {
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if u.rank[ra] < u.rank[rb] {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	if u.rank[ra] == u.rank[rb] {
		u.rank[ra]++
	}
}

func WeldAndCanonicalize(in mesh.Mesh, opt WeldOptions) (mesh.Mesh, WeldReport, *errs.CodedError) {
	if opt.Tolerance < 0 || math.IsNaN(opt.Tolerance) || math.IsInf(opt.Tolerance, 0) {
		return mesh.Mesh{}, WeldReport{}, errs.New(errs.CodeTolo,
			"weld tolerance must be finite and non-negative, got %g", opt.Tolerance)
	}
	if opt.MinComponentPct <= 0 || opt.MinComponentPct >= 1 {
		opt.MinComponentPct = 0.05
	}
	suggested := suggestTolerance(in)
	uf := findWeldClusters(in, opt.Tolerance)
	roots := make(map[int]bool)
	for i := range in.Vertices {
		roots[uf.find(i)] = true
	}
	report := WeldReport{
		Tolerance:          opt.Tolerance,
		SuggestedTolerance: suggested,
		InputVertices:      len(in.Vertices),
		MergedVertices:     len(in.Vertices) - len(roots),
	}
	if report.MergedVertices < 0 {
		report.MergedVertices = 0
	}

	// Stable representative order keeps original baseline indices first. This
	// makes generated known-point indices valid after welding.
	rootList := make([]int, 0, len(roots))
	for root := range roots {
		rootList = append(rootList, root)
	}
	sort.Ints(rootList)
	newIndex := make(map[int]int, len(rootList))
	out := mesh.Mesh{Vertices: make([]mesh.Vec3, len(rootList))}
	for i, root := range rootList {
		newIndex[root] = i
		out.Vertices[i] = in.Vertices[root]
	}
	oldToNew := make([]int, len(in.Vertices))
	for i := range in.Vertices {
		oldToNew[i] = newIndex[uf.find(i)]
	}
	for fi, f := range in.Faces {
		a, b, c := oldToNew[f.A], oldToNew[f.B], oldToNew[f.C]
		if a == b || b == c || c == a {
			e := errs.New(errs.CodeTolo,
				"tolerance %.9g collapsed face %d into a repeated-index face", opt.Tolerance, fi)
			e.SuggestedTolerance = suggested
			return mesh.Mesh{}, report, errs.WithFace(e, fi)
		}
		out.Faces = append(out.Faces, mesh.Face{A: a, B: b, C: c})
	}
	for fi := range out.Faces {
		if area := out.FaceArea(fi); area <= 1e-14 || math.IsNaN(area) {
			e := errs.New(errs.CodeTolo,
				"tolerance %.9g collapsed face %d to zero area", opt.Tolerance, fi)
			e.SuggestedTolerance = suggested
			return mesh.Mesh{}, report, errs.WithFace(e, fi)
		}
	}

	out, removedDup := removeDuplicateFaces(out)
	report.DuplicateFacesRemoved = removedDup
	out, removedNM := removeNonManifoldFaces(out)
	report.NonManifoldFacesRemoved = removedNM
	out, compsRemoved, compFaces := removeAuxiliaryComponents(out, opt.MinComponentPct)
	report.ComponentsRemoved = compsRemoved
	report.ComponentFacesRemoved = compFaces
	out = compactVertices(out)
	report.OutputVertices = len(out.Vertices)
	report.FinalCompactVertices = len(out.Vertices)

	boundary := mesh.BoundaryLoops(out)
	for _, loop := range boundary.Loops {
		if len(loop.Vertices) < 3 {
			e := errs.New(errs.CodeTolo,
				"weld tolerance %.9g left a degenerate boundary loop of %d vertices",
				opt.Tolerance, len(loop.Vertices))
			e.SuggestedTolerance = suggested
			return mesh.Mesh{}, report, e
		}
	}
	return out, report, nil
}

func suggestTolerance(m mesh.Mesh) float64 {
	minEdge := math.Inf(1)
	for _, f := range m.Faces {
		for _, e := range [][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}} {
			if e[0] < 0 || e[0] >= len(m.Vertices) || e[1] < 0 || e[1] >= len(m.Vertices) {
				continue
			}
			d := m.Vertices[e[0]].Distance(m.Vertices[e[1]])
			if d > 1e-15 && d < minEdge {
				minEdge = d
			}
		}
	}
	if math.IsInf(minEdge, 1) {
		return 1e-9
	}
	v := minEdge * 0.25
	if v < 1e-12 {
		return 1e-12
	}
	return v
}

func findWeldClusters(m mesh.Mesh, tol float64) *unionFind {
	uf := newUnionFind(len(m.Vertices))
	if tol == 0 {
		type key [3]uint64
		seen := make(map[key]int, len(m.Vertices))
		mk := func(v mesh.Vec3) key {
			return key{math.Float64bits(v.X), math.Float64bits(v.Y), math.Float64bits(v.Z)}
		}
		for i, v := range m.Vertices {
			k := mk(v)
			if first, ok := seen[k]; ok {
				uf.union(first, i)
			} else {
				seen[k] = i
			}
		}
		return uf
	}
	type cell struct{ x, y, z int64 }
	buckets := make(map[cell][]int, len(m.Vertices))
	for i, v := range m.Vertices {
		c := cell{
			x: int64(math.Floor(v.X / tol)),
			y: int64(math.Floor(v.Y / tol)),
			z: int64(math.Floor(v.Z / tol)),
		}
		candidates := make([]int, 0, 8)
		for dx := int64(-1); dx <= 1; dx++ {
			for dy := int64(-1); dy <= 1; dy++ {
				for dz := int64(-1); dz <= 1; dz++ {
					candidates = append(candidates, buckets[cell{c.x + dx, c.y + dy, c.z + dz}]...)
				}
			}
		}
		sort.Ints(candidates)
		for _, j := range candidates {
			if i != j && m.Vertices[i].Distance(m.Vertices[j]) <= tol {
				uf.union(j, i)
			}
		}
		buckets[c] = append(buckets[c], i)
	}
	return uf
}

func removeDuplicateFaces(m mesh.Mesh) (mesh.Mesh, int) {
	seen := make(map[[3]int]bool, len(m.Faces))
	out := m.Clone()
	out.Faces = out.Faces[:0]
	removed := 0
	for _, f := range m.Faces {
		ids := f.Indices()
		sort.Ints(ids[:])
		k := ids
		if seen[k] {
			removed++
			continue
		}
		seen[k] = true
		out.Faces = append(out.Faces, f)
	}
	return out, removed
}

func removeNonManifoldFaces(m mesh.Mesh) (mesh.Mesh, int) {
	out := m.Clone()
	removed := 0
	for iter := 0; iter < len(m.Faces)+1; iter++ {
		topo := mesh.BuildTopology(out)
		var bad *mesh.EdgeRecord
		for _, e := range topo.SortedEdges() {
			if len(e.Uses) > 2 {
				bad = e
				break
			}
		}
		if bad == nil {
			break
		}
		bestFace, bestScore := -1, -1
		for _, use := range bad.Uses {
			fi := use.Face
			score := boundaryEdgeScore(out, topo, fi)
			if score > bestScore || (score == bestScore && fi > bestFace) {
				bestFace, bestScore = fi, score
			}
		}
		if bestFace < 0 {
			break
		}
		out.Faces = append(out.Faces[:bestFace], out.Faces[bestFace+1:]...)
		removed++
	}
	return out, removed
}

func boundaryEdgeScore(m mesh.Mesh, topo mesh.Topology, face int) int {
	f := m.Faces[face]
	score := 0
	for _, e := range [][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}} {
		if rec := topo.Edges[mesh.MakeEdge(e[0], e[1])]; rec != nil && len(rec.Uses) == 1 {
			score++
		}
	}
	return score
}

func removeAuxiliaryComponents(m mesh.Mesh, minPct float64) (mesh.Mesh, int, int) {
	comps := mesh.FaceComponents(m, mesh.ComponentAllSharedEdges)
	if len(comps) <= 1 {
		return m, 0, 0
	}
	topo := mesh.BuildTopology(m)
	compStats := make([]struct {
		closed bool
		vol    float64
	}, len(comps))
	faceComp := make([]int, len(m.Faces))
	for ci, c := range comps {
		compStats[ci].closed = true
		for _, fi := range c.Faces {
			faceComp[fi] = ci
			f := m.Faces[fi]
			a, b, cc := m.Vertices[f.A], m.Vertices[f.B], m.Vertices[f.C]
			compStats[ci].vol += a.Dot(b.Cross(cc)) / 6
			for _, e := range [][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}} {
				if len(topo.Edges[mesh.MakeEdge(e[0], e[1])].Uses) != 2 {
					compStats[ci].closed = false
				}
			}
		}
	}
	maxFaces := len(comps[0].Faces)
	drop := make([]bool, len(comps))
	removedComps, removedFaces := 0, 0
	for ci, c := range comps {
		if ci == 0 {
			continue
		}
		ratio := float64(len(c.Faces)) / float64(maxFaces)
		if compStats[ci].closed && math.Abs(compStats[ci].vol) > 1e-12 {
			continue
		}
		if ratio >= minPct {
			continue
		}
		drop[ci] = true
		removedComps++
		removedFaces += len(c.Faces)
	}
	if removedFaces == 0 {
		return m, 0, 0
	}
	out := m.Clone()
	out.Faces = out.Faces[:0]
	for fi, f := range m.Faces {
		if !drop[faceComp[fi]] {
			out.Faces = append(out.Faces, f)
		}
	}
	return out, removedComps, removedFaces
}

func compactVertices(m mesh.Mesh) mesh.Mesh {
	used := make([]bool, len(m.Vertices))
	for _, f := range m.Faces {
		for _, vi := range f.Indices() {
			if vi >= 0 && vi < len(used) {
				used[vi] = true
			}
		}
	}
	remap := make([]int, len(m.Vertices))
	for i := range remap {
		remap[i] = -1
	}
	out := mesh.Mesh{}
	for i, u := range used {
		if u {
			remap[i] = len(out.Vertices)
			out.Vertices = append(out.Vertices, m.Vertices[i])
		}
	}
	out.Faces = make([]mesh.Face, len(m.Faces))
	for fi, f := range m.Faces {
		out.Faces[fi] = mesh.Face{A: remap[f.A], B: remap[f.B], C: remap[f.C]}
	}
	return out
}

func validateTruthTopology(m mesh.Mesh, truth generator.Truth, stage string) *errs.CodedError {
	topo := mesh.BuildTopology(m)
	euler := len(m.Vertices) - len(topo.Edges) + len(m.Faces)
	if len(m.Vertices) != truth.Repaired.Vertices ||
		len(m.Faces) != truth.Repaired.Faces ||
		euler != truth.Repaired.Euler {
		return errs.New(errs.CodeGeom,
			"%s topology V=%d F=%d E=%d chi=%d does not match truth V=%d F=%d chi=%d",
			stage, len(m.Vertices), len(m.Faces), len(topo.Edges), euler,
			truth.Repaired.Vertices, truth.Repaired.Faces, truth.Repaired.Euler)
	}
	return nil
}

func debugCounts(m mesh.Mesh) string {
	t := mesh.BuildTopology(m)
	return fmt.Sprintf("V=%d F=%d E=%d", len(m.Vertices), len(m.Faces), len(t.Edges))
}
