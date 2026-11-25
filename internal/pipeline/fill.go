package pipeline

import (
	"math"
	"sort"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
)

type FilledRing struct {
	Index     int         `json:"index"`
	Vertices  []int       `json:"vertices"`
	Triangles []mesh.Face `json:"triangles"`
	Method    string      `json:"method"`
	BestScore float64     `json:"best_score"`
}

type FillReport struct {
	BoundaryLoops  int          `json:"boundary_loops"`
	FilledRings    []FilledRing `json:"filled_rings"`
	TrianglesAdded int          `json:"triangles_added"`
	SeamVertices   []int        `json:"seam_vertices"`
}

func FillHoles(input *mesh.Mesh) (*mesh.Mesh, FillReport, error) {
	var report FillReport
	out := input.Clone()
	adj := mesh.BuildAdjacency(out)
	report.BoundaryLoops = len(adj.BoundaryLoop)
	beforeNonManifold := adj.Stats.NonManifoldEdges
	seamSet := map[int]bool{}
	edgeCounts := map[mesh.Edge]int{}
	for e, faces := range adj.EdgeFaces {
		edgeCounts[e] = len(faces)
	}

	for ringIndex, loop := range adj.BoundaryLoop {
		unique := map[int]bool{}
		for _, v := range loop.Vertices {
			unique[v] = true
		}
		if !loop.Closed || len(loop.Vertices) < 3 || len(unique) < 3 {
			coords := ringCoordinates(out, loop.Vertices)
			return nil, report, holeError(ringIndex, "boundary ring is degenerate", coords, map[string]any{
				"closed": loop.Closed, "vertex_count": len(loop.Vertices),
			})
		}

		tris, method, score, err := triangulateLoop(out, loop.Vertices, adj, edgeCounts)
		if err != nil {
			coords := ringCoordinates(out, loop.Vertices)
			return nil, report, holeError(ringIndex, err.Error(), coords, nil)
		}
		fr := FilledRing{Index: ringIndex, Vertices: append([]int(nil), loop.Vertices...), Triangles: tris, Method: method, BestScore: score}
		report.FilledRings = append(report.FilledRings, fr)
		report.TrianglesAdded += len(tris)
		out.Faces = append(out.Faces, tris...)
		for _, v := range loop.Vertices {
			seamSet[v] = true
		}
	}

	after := mesh.BuildAdjacency(out)
	for _, fr := range report.FilledRings {
		if err := VerifyRingClosure(out, fr); err != nil {
			return nil, report, err
		}
	}
	if after.Stats.NonManifoldEdges > beforeNonManifold {
		return nil, report, errs.New(errs.EHOLE, "filling introduced %d new non-manifold edge(s)", after.Stats.NonManifoldEdges-beforeNonManifold)
	}

	for v := range seamSet {
		report.SeamVertices = append(report.SeamVertices, v)
	}
	sort.Ints(report.SeamVertices)
	return out, report, nil
}

func VerifyRingClosure(m *mesh.Mesh, fr FilledRing) error {
	adj := mesh.BuildAdjacency(m)
	n := len(fr.Vertices)
	for i := 0; i < n; i++ {
		a := fr.Vertices[i]
		b := fr.Vertices[(i+1)%n]
		e := mesh.NewEdge(a, b)
		incidences := adj.EdgeFaces[e]
		if len(incidences) < 2 {
			coords := ringCoordinates(m, fr.Vertices)
			return holeError(fr.Index, "target region is not closed after filling", coords, map[string]any{
				"ring": fr.Index, "edge": []int{a, b}, "incident_faces": len(incidences),
			})
		}
		if len(incidences) > 2 {
			coords := ringCoordinates(m, fr.Vertices)
			return holeError(fr.Index, "filling created a non-manifold target edge", coords, map[string]any{
				"ring": fr.Index, "edge": []int{a, b}, "incident_faces": len(incidences),
			})
		}
	}
	return nil
}

func holeError(ring int, msg string, coords []mesh.Vec3, extra map[string]any) error {
	details := map[string]any{"ring": ring, "ring_coordinates": coords}
	for k, v := range extra {
		details[k] = v
	}
	err := errs.New(errs.EHOLE, "ring %d: %s", ring, msg)
	return errs.WithDetails(err, details)
}

func ringCoordinates(m *mesh.Mesh, indices []int) []mesh.Vec3 {
	coords := make([]mesh.Vec3, 0, len(indices))
	for _, idx := range indices {
		if idx >= 0 && idx < len(m.Vertices) {
			coords = append(coords, m.Vertices[idx])
		}
	}
	return coords
}

type point2 struct{ x, y float64 }

func triangulateLoop(m *mesh.Mesh, loop []int, adj *mesh.Adjacency, edgeCounts map[mesh.Edge]int) ([]mesh.Face, string, float64, error) {
	if len(loop) == 3 {
		t := mesh.Face{A: loop[0], B: loop[1], C: loop[2]}
		for _, e := range [3]mesh.Edge{mesh.NewEdge(t.A, t.B), mesh.NewEdge(t.B, t.C), mesh.NewEdge(t.C, t.A)} {
			edgeCounts[e]++
		}
		return []mesh.Face{t}, "triangle", 1, nil
	}
	normal := polygonNormal(m.Vertices, loop)
	if mesh.Norm(normal) < 1e-12 {
		return nil, "", 0, errs.New("E_FILL", "ring plane is degenerate")
	}
	normal = mesh.Normalized(normal)
	helper := mesh.Vec3{0, 0, 1}
	if math.Abs(mesh.Dot(helper, normal)) > 0.9 {
		helper = mesh.Vec3{1, 0, 0}
	}
	u := mesh.Normalized(mesh.Cross(helper, normal))
	vAxis := mesh.Cross(normal, u)
	project := func(idx int) point2 {
		p := m.Vertices[idx]
		return point2{mesh.Dot(p, u), mesh.Dot(p, vAxis)}
	}

	remaining := append([]int(nil), loop...)
	var tris []mesh.Face
	bestTotal := 0.0
	method := "normal-continuity-ear-clipping"

	for len(remaining) > 3 {
		best := -1
		bestScore := -math.MaxFloat64
		for i := range remaining {
			prev := remaining[(i-1+len(remaining))%len(remaining)]
			cur := remaining[i]
			next := remaining[(i+1)%len(remaining)]
			a, b, c := project(prev), project(cur), project(next)
			convex := cross2(a, b, c) > 1e-12

			blocked := false
			for j, otherIdx := range remaining {
				if j == i || j == (i-1+len(remaining))%len(remaining) || j == (i+1)%len(remaining) {
					continue
				}
				if pointInTriangle2(project(otherIdx), a, b, c) {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			candidateFace := mesh.Face{A: prev, B: cur, C: next}
			unsafe := false
			for _, e := range [3]mesh.Edge{
				mesh.NewEdge(candidateFace.A, candidateFace.B),
				mesh.NewEdge(candidateFace.B, candidateFace.C),
				mesh.NewEdge(candidateFace.C, candidateFace.A),
			} {
				if edgeCounts[e] >= 2 {
					unsafe = true
					break
				}
			}
			if unsafe {
				continue
			}

			faceNormal := mesh.FaceNormal(m, mesh.Face{A: prev, B: cur, C: next})
			continuity := normalContinuity(m, faceNormal, []int{prev, cur, next}, adj)
			score := continuity
			if convex {
				score += 2
			} else {
				score -= 2
			}
			if score > bestScore {
				bestScore = score
				best = i
			}
		}

		if best < 0 {
			return nil, "", 0, errs.New(errs.EHOLE, "ring %v has no safe ear triangulation", loop)
		}
		prev := remaining[(best-1+len(remaining))%len(remaining)]
		cur := remaining[best]
		next := remaining[(best+1)%len(remaining)]
		tris = append(tris, mesh.Face{A: prev, B: cur, C: next})
		for _, e := range [3]mesh.Edge{mesh.NewEdge(prev, cur), mesh.NewEdge(cur, next), mesh.NewEdge(next, prev)} {
			edgeCounts[e]++
		}
		bestTotal += bestScore
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	tris = append(tris, mesh.Face{A: remaining[0], B: remaining[1], C: remaining[2]})
	return tris, method, bestTotal / float64(len(tris)), nil
}

func normalContinuity(m *mesh.Mesh, candidate mesh.Vec3, verts []int, adj *mesh.Adjacency) float64 {
	sum := 0.0
	count := 0
	for _, v := range verts {
		var normal mesh.Vec3
		n := 0
		for _, fi := range adj.VertexFaces[v] {
			normal = mesh.Add(normal, mesh.FaceNormal(m, m.Faces[fi]))
			n++
		}
		if n > 0 {
			avg := mesh.Scale(normal, 1/float64(n))
			if norm := mesh.Norm(avg); norm > 0 {
				sum += mesh.Dot(candidate, mesh.Scale(avg, 1/norm))
				count++
			}
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

func polygonNormal(vertices []mesh.Vec3, loop []int) mesh.Vec3 {
	var n mesh.Vec3
	for i := range loop {
		cur := vertices[loop[i]]
		next := vertices[loop[(i+1)%len(loop)]]
		n[0] += (cur[1] - next[1]) * (cur[2] + next[2])
		n[1] += (cur[2] - next[2]) * (cur[0] + next[0])
		n[2] += (cur[0] - next[0]) * (cur[1] + next[1])
	}
	return n
}

func cross2(a, b, c point2) float64 {
	return (b.x-a.x)*(c.y-a.y) - (b.y-a.y)*(c.x-a.x)
}

func sign2(p, a, b point2) float64 {
	return (p.x-b.x)*(a.y-b.y) - (a.x-b.x)*(p.y-b.y)
}

func pointInTriangle2(p, a, b, c point2) bool {
	d1 := sign2(p, a, b)
	d2 := sign2(p, b, c)
	d3 := sign2(p, c, a)
	hasNeg := d1 < -1e-10 || d2 < -1e-10 || d3 < -1e-10
	hasPos := d1 > 1e-10 || d2 > 1e-10 || d3 > 1e-10
	return !(hasNeg && hasPos)
}
