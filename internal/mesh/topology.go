package mesh

import "sort"

type Edge [2]int

func NewEdge(a, b int) Edge {
	if a > b {
		a, b = b, a
	}
	return Edge{a, b}
}

type TopologyStats struct {
	Vertices            int     `json:"vertices"`
	Faces               int     `json:"faces"`
	Edges               int     `json:"edges"`
	Euler               int     `json:"euler"`
	Genus               float64 `json:"genus"`
	BoundaryEdges       int     `json:"boundary_edges"`
	NonManifoldEdges    int     `json:"non_manifold_edges"`
	BoundaryLoops       int     `json:"boundary_loops"`
	DegenerateFaces     int     `json:"degenerate_faces"`
	ConnectedComponents int     `json:"connected_components"`
	LargestComponent    int     `json:"largest_component"`
	SelfIntersections   int     `json:"self_intersections,omitempty"`
}

type Loop struct {
	Vertices []int `json:"vertices"`
	Closed   bool  `json:"closed"`
}

type Adjacency struct {
	EdgeFaces    map[Edge][]int
	VertexFaces  [][]int
	FaceEdges    [][3]Edge
	Stats        TopologyStats
	BoundaryLoop []Loop
}

func BuildAdjacency(m *Mesh) *Adjacency {
	edgeFaces := map[Edge][]int{}
	vertexFaces := make([][]int, len(m.Vertices))
	faceEdges := make([][3]Edge, len(m.Faces))

	for fi, f := range m.Faces {
		idx := f.Indices()
		faceEdges[fi] = [3]Edge{NewEdge(idx[0], idx[1]), NewEdge(idx[1], idx[2]), NewEdge(idx[2], idx[0])}
		for _, e := range faceEdges[fi] {
			edgeFaces[e] = append(edgeFaces[e], fi)
		}
		for _, vi := range idx {
			vertexFaces[vi] = append(vertexFaces[vi], fi)
		}
	}

	loops := BoundaryLoops(m, edgeFaces)
	closedLoops := 0
	boundaryEdges := 0
	nonManifold := 0
	for _, faces := range edgeFaces {
		switch len(faces) {
		case 1:
			boundaryEdges++
		case 2:
		default:
			if len(faces) > 2 {
				nonManifold++
			}
		}
	}
	for _, l := range loops {
		if l.Closed {
			closedLoops++
		}
	}

	components, largest := FaceComponents(m, edgeFaces)
	edges := len(edgeFaces)
	euler := len(m.Vertices) - edges + len(m.Faces)
	stats := TopologyStats{
		Vertices:            len(m.Vertices),
		Faces:               len(m.Faces),
		Edges:               edges,
		Euler:               euler,
		BoundaryEdges:       boundaryEdges,
		NonManifoldEdges:    nonManifold,
		BoundaryLoops:       closedLoops,
		DegenerateFaces:     len(m.DegenerateFaces(1e-12)),
		ConnectedComponents: components,
		LargestComponent:    largest,
	}
	if euler%2 == 0 {
		stats.Genus = float64(2-euler) / 2
	}

	return &Adjacency{EdgeFaces: edgeFaces, VertexFaces: vertexFaces, FaceEdges: faceEdges, Stats: stats, BoundaryLoop: loops}
}

func Analyze(m *Mesh) TopologyStats {
	if m == nil {
		return TopologyStats{}
	}
	return BuildAdjacency(m).Stats
}

func BoundaryLoops(m *Mesh, edgeFaces map[Edge][]int) []Loop {
	if edgeFaces == nil {
		edgeFaces = map[Edge][]int{}
		for fi, f := range m.Faces {
			for _, e := range [][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}} {
				key := NewEdge(e[0], e[1])
				edgeFaces[key] = append(edgeFaces[key], fi)
			}
		}
	}

	boundary := map[Edge]bool{}
	adj := map[int][]int{}
	var keys []Edge
	for e, faces := range edgeFaces {
		if len(faces) == 1 {
			boundary[e] = true
			keys = append(keys, e)
			adj[e[0]] = append(adj[e[0]], e[1])
			adj[e[1]] = append(adj[e[1]], e[0])
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for v := range adj {
		sort.Ints(adj[v])
	}

	used := map[Edge]bool{}
	var loops []Loop
	for _, first := range keys {
		if used[first] {
			continue
		}
		start, next := first[0], first[1]
		loop := []int{start}
		prev, cur := start, next
		used[first] = true
		closed := false

		for steps := 0; steps <= len(m.Vertices)+2; steps++ {
			if cur == start {
				closed = true
				break
			}
			loop = append(loop, cur)
			candidates := adj[cur]
			chosen := -1
			for _, cand := range candidates {
				if cand == prev {
					continue
				}
				e := NewEdge(cur, cand)
				if used[e] {
					continue
				}
				chosen = cand
				break
			}
			if chosen < 0 {
				break
			}
			used[NewEdge(cur, chosen)] = true
			prev, cur = cur, chosen
		}
		loops = append(loops, Loop{Vertices: loop, Closed: closed})
	}
	return loops
}

// FaceComponents returns the number of face components connected across edges
// and the number of faces in the largest component.
func FaceComponents(m *Mesh, edgeFaces map[Edge][]int) (int, int) {
	if len(m.Faces) == 0 {
		return 0, 0
	}
	if edgeFaces == nil {
		edgeFaces = map[Edge][]int{}
		for fi, f := range m.Faces {
			for _, e := range [][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}} {
				key := NewEdge(e[0], e[1])
				edgeFaces[key] = append(edgeFaces[key], fi)
			}
		}
	}
	neighbors := make([][]int, len(m.Faces))
	for _, faces := range edgeFaces {
		if len(faces) < 2 {
			continue
		}
		for i := 0; i < len(faces); i++ {
			for j := i + 1; j < len(faces); j++ {
				a, b := faces[i], faces[j]
				neighbors[a] = append(neighbors[a], b)
				neighbors[b] = append(neighbors[b], a)
			}
		}
	}

	seen := make([]bool, len(m.Faces))
	count := 0
	largest := 0
	for root := range m.Faces {
		if seen[root] {
			continue
		}
		count++
		size := 0
		queue := []int{root}
		seen[root] = true
		for len(queue) > 0 {
			fi := queue[0]
			queue = queue[1:]
			size++
			for _, ni := range neighbors[fi] {
				if !seen[ni] {
					seen[ni] = true
					queue = append(queue, ni)
				}
			}
		}
		if size > largest {
			largest = size
		}
	}
	return count, largest
}
