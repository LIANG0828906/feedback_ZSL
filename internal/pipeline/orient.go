package pipeline

import (
	"sort"

	"meshdoctor/internal/mesh"
)

type OrientReport struct {
	Components    int `json:"components"`
	LargestSize   int `json:"largest_size"`
	ReversedFaces int `json:"reversed_faces"`
}

func OrientConsistent(input *mesh.Mesh) (*mesh.Mesh, OrientReport) {
	out := input.Clone()
	adj := mesh.BuildAdjacency(out)
	report := OrientReport{}

	components := faceEdgeComponents(out, adj.EdgeFaces)
	report.Components = len(components)
	for _, comp := range components {
		if len(comp) > report.LargestSize {
			report.LargestSize = len(comp)
		}
	}

	center := mesh.Vec3{}
	if len(out.Vertices) > 0 {
		for _, v := range out.Vertices {
			center = mesh.Add(center, v)
		}
		center = mesh.Scale(center, 1/float64(len(out.Vertices)))
	}

	oriented := make([]bool, len(out.Faces))
	for _, comp := range components {
		if len(comp) == 0 {
			continue
		}
		root := comp[0]
		centroid := faceCentroid(out, out.Faces[root])
		n := mesh.FaceNormal(out, out.Faces[root])
		if mesh.Dot(n, mesh.Sub(centroid, center)) < 0 {
			reverseFace(out, root)
			report.ReversedFaces++
		}
		oriented[root] = true
		queue := append([]int(nil), comp[0])
		inComponent := map[int]bool{}
		for _, fi := range comp {
			inComponent[fi] = true
		}

		for len(queue) > 0 {
			fi := queue[0]
			queue = queue[1:]
			f := out.Faces[fi]
			directed := [3][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}}
			for _, de := range directed {
				e := mesh.NewEdge(de[0], de[1])
				for _, ni := range adj.EdgeFaces[e] {
					if ni == fi || !inComponent[ni] || oriented[ni] {
						continue
					}
					if faceHasDirectedEdge(out.Faces[ni], de[0], de[1]) {
						reverseFace(out, ni)
						report.ReversedFaces++
					}
					oriented[ni] = true
					queue = append(queue, ni)
				}
			}
		}
	}

	return out, report
}

func faceEdgeComponents(m *mesh.Mesh, edgeFaces map[mesh.Edge][]int) [][]int {
	adj := make([][]int, len(m.Faces))
	for _, faces := range edgeFaces {
		if len(faces) < 2 {
			continue
		}
		for i := 0; i < len(faces); i++ {
			for j := i + 1; j < len(faces); j++ {
				a, b := faces[i], faces[j]
				adj[a] = append(adj[a], b)
				adj[b] = append(adj[b], a)
			}
		}
	}
	seen := make([]bool, len(m.Faces))
	var comps [][]int
	for root := range m.Faces {
		if seen[root] {
			continue
		}
		var comp []int
		queue := []int{root}
		seen[root] = true
		for len(queue) > 0 {
			fi := queue[0]
			queue = queue[1:]
			comp = append(comp, fi)
			for _, ni := range adj[fi] {
				if !seen[ni] {
					seen[ni] = true
					queue = append(queue, ni)
				}
			}
		}
		sort.Ints(comp)
		comps = append(comps, comp)
	}
	sort.Slice(comps, func(i, j int) bool {
		if len(comps[i]) != len(comps[j]) {
			return len(comps[i]) > len(comps[j])
		}
		return comps[i][0] < comps[j][0]
	})
	return comps
}

func reverseFace(m *mesh.Mesh, fi int) {
	f := m.Faces[fi]
	m.Faces[fi] = mesh.Face{A: f.A, B: f.C, C: f.B}
}

func faceHasDirectedEdge(f mesh.Face, a, b int) bool {
	edges := [3][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}}
	for _, e := range edges {
		if e[0] == a && e[1] == b {
			return true
		}
	}
	return false
}

func faceCentroid(m *mesh.Mesh, f mesh.Face) mesh.Vec3 {
	s := mesh.Add(mesh.Add(m.Vertices[f.A], m.Vertices[f.B]), m.Vertices[f.C])
	return mesh.Scale(s, 1/3)
}

type SmoothReport struct {
	Weight     float64 `json:"weight"`
	Iterations int     `json:"iterations"`
	Pinned     int     `json:"pinned_vertices"`
}

func Smooth(input *mesh.Mesh, weight float64, iterations int, pinned []int) (*mesh.Mesh, SmoothReport) {
	pin := map[int]bool{}
	for _, v := range pinned {
		pin[v] = true
	}
	neighbors := vertexNeighbors(input)
	current := input.Clone()

	for iter := 0; iter < iterations; iter++ {
		next := current.Clone()
		for i := range current.Vertices {
			if pin[i] || len(neighbors[i]) == 0 {
				continue
			}
			avg := mesh.Vec3{}
			for _, j := range neighbors[i] {
				avg = mesh.Add(avg, current.Vertices[j])
			}
			avg = mesh.Scale(avg, 1/float64(len(neighbors[i])))
			delta := mesh.Sub(avg, current.Vertices[i])
			next.Vertices[i] = mesh.Add(current.Vertices[i], mesh.Scale(delta, weight))
		}
		current = next
	}
	return current, SmoothReport{Weight: weight, Iterations: iterations, Pinned: len(pin)}
}

func vertexNeighbors(m *mesh.Mesh) [][]int {
	set := make([]map[int]bool, len(m.Vertices))
	for i := range set {
		set[i] = map[int]bool{}
	}
	for _, f := range m.Faces {
		idx := f.Indices()
		for i := 0; i < 3; i++ {
			a, b := idx[i], idx[(i+1)%3]
			set[a][b] = true
			set[b][a] = true
		}
	}
	out := make([][]int, len(m.Vertices))
	for v := range set {
		out[v] = make([]int, 0, len(set[v]))
		for n := range set[v] {
			out[v] = append(out[v], n)
		}
		sort.Ints(out[v])
	}
	return out
}
