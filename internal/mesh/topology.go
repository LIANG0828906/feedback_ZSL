package mesh

import (
	"math"
	"sort"
)

type BoundaryLoop struct {
	Vertices []int   `json:"vertices"`
	Points   []Vec3  `json:"points"`
	Area     float64 `json:"projected_area"`
}

type BoundaryAnalysis struct {
	Loops      []BoundaryLoop `json:"loops"`
	OpenChains int            `json:"open_chains"`
}

// BoundaryLoops connects boundary edges into deterministic closed rings. A
// component with a vertex degree other than two is reported as an open chain,
// which is useful for diagnosing damaged or non-manifold add-on sheets.
func BoundaryLoops(m Mesh) BoundaryAnalysis {
	t := BuildTopology(m)
	type bend struct {
		key EdgeKey
		e   *EdgeRecord
	}
	var boundary []bend
	for _, e := range t.SortedEdges() {
		if len(e.Uses) == 1 {
			boundary = append(boundary, bend{key: EdgeKey{A: e.A, B: e.B}, e: e})
		}
	}
	if len(boundary) == 0 {
		return BoundaryAnalysis{}
	}

	keyToEdge := make(map[EdgeKey]*EdgeRecord, len(boundary))
	adj := make(map[int][]EdgeKey, len(boundary))
	for _, b := range boundary {
		keyToEdge[b.key] = b.e
		adj[b.key.A] = append(adj[b.key.A], b.key)
		adj[b.key.B] = append(adj[b.key.B], b.key)
	}
	for v := range adj {
		sort.Slice(adj[v], func(i, j int) bool {
			if adj[v][i].A != adj[v][j].A {
				return adj[v][i].A < adj[v][j].A
			}
			return adj[v][i].B < adj[v][j].B
		})
	}

	visited := make(map[EdgeKey]bool, len(boundary))
	keys := make([]EdgeKey, 0, len(boundary))
	for k := range keyToEdge {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].A != keys[j].A {
			return keys[i].A < keys[j].A
		}
		return keys[i].B < keys[j].B
	})

	var out BoundaryAnalysis
	for _, start := range keys {
		if visited[start] {
			continue
		}
		componentEdges, componentVerts := collectBoundaryComponent(start, adj, visited)
		closed := len(componentVerts) > 0
		for _, v := range componentVerts {
			if len(adj[v]) != 2 {
				closed = false
				break
			}
		}
		if len(componentEdges) != len(componentVerts) {
			closed = false
		}
		if !closed {
			out.OpenChains++
			continue
		}
		order := walkBoundaryLoop(start, keyToEdge, adj)
		if len(order) < 3 {
			out.OpenChains++
			continue
		}
		loop := BoundaryLoop{Vertices: order}
		pts := make([]Vec3, len(order))
		for i, vi := range order {
			pts[i] = m.Vertices[vi]
		}
		loop.Points = pts
		loop.Area = loopProjectedArea(pts)
		out.Loops = append(out.Loops, loop)
	}
	sort.Slice(out.Loops, func(i, j int) bool {
		return out.Loops[i].Vertices[0] < out.Loops[j].Vertices[0]
	})
	return out
}

func collectBoundaryComponent(start EdgeKey, adj map[int][]EdgeKey, visited map[EdgeKey]bool) ([]EdgeKey, []int) {
	queue := []EdgeKey{start}
	visited[start] = true
	var edges []EdgeKey
	verts := make(map[int]bool)
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		edges = append(edges, k)
		verts[k.A] = true
		verts[k.B] = true
		for _, v := range []int{k.A, k.B} {
			for _, n := range adj[v] {
				if !visited[n] {
					visited[n] = true
					queue = append(queue, n)
				}
			}
		}
	}
	vs := make([]int, 0, len(verts))
	for v := range verts {
		vs = append(vs, v)
	}
	sort.Ints(vs)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].A != edges[j].A {
			return edges[i].A < edges[j].A
		}
		return edges[i].B < edges[j].B
	})
	return edges, vs
}

func walkBoundaryLoop(start EdgeKey, keyToEdge map[EdgeKey]*EdgeRecord, adj map[int][]EdgeKey) []int {
	use := keyToEdge[start].Uses[0]
	startV, current := use.From, use.To
	order := []int{startV}
	used := make(map[EdgeKey]bool)
	used[start] = true
	for steps := 0; steps <= len(keyToEdge)+1; steps++ {
		order = append(order, current)
		if current == startV {
			order = order[:len(order)-1]
			return order
		}
		var next EdgeKey
		found := false
		for _, k := range adj[current] {
			if !used[k] {
				next, found = k, true
				break
			}
		}
		if !found {
			return nil
		}
		used[next] = true
		if next.A == current {
			current = next.B
		} else {
			current = next.A
		}
	}
	return nil
}

func loopProjectedArea(pts []Vec3) float64 {
	if len(pts) < 3 {
		return 0
	}
	n := Vec3{}
	for i := range pts {
		a, b := pts[i], pts[(i+1)%len(pts)]
		n.X += (a.Y - b.Y) * (a.Z + b.Z)
		n.Y += (a.Z - b.Z) * (a.X + b.X)
		n.Z += (a.X - b.X) * (a.Y + b.Y)
	}
	return n.Len() * 0.5
}

type ComponentMode int

const (
	ComponentAllSharedEdges ComponentMode = iota
	ComponentManifoldEdges
)

type FaceComponent struct {
	ID    int
	Faces []int
}

func FaceComponents(m Mesh, mode ComponentMode) []FaceComponent {
	t := BuildTopology(m)
	neighbors := make([][]int, len(m.Faces))
	for _, e := range t.SortedEdges() {
		include := false
		switch mode {
		case ComponentAllSharedEdges:
			include = len(e.Uses) >= 2
		case ComponentManifoldEdges:
			include = len(e.Uses) == 2
		}
		if !include {
			continue
		}
		for i := 0; i < len(e.Uses); i++ {
			for j := i + 1; j < len(e.Uses); j++ {
				a, b := e.Uses[i].Face, e.Uses[j].Face
				neighbors[a] = append(neighbors[a], b)
				neighbors[b] = append(neighbors[b], a)
			}
		}
	}
	for i := range neighbors {
		sort.Ints(neighbors[i])
		neighbors[i] = uniqueInts(neighbors[i])
	}
	seen := make([]bool, len(m.Faces))
	var comps []FaceComponent
	for root := range m.Faces {
		if seen[root] {
			continue
		}
		comp := FaceComponent{ID: len(comps)}
		queue := []int{root}
		seen[root] = true
		for len(queue) > 0 {
			f := queue[0]
			queue = queue[1:]
			comp.Faces = append(comp.Faces, f)
			for _, nb := range neighbors[f] {
				if !seen[nb] {
					seen[nb] = true
					queue = append(queue, nb)
				}
			}
		}
		sort.Ints(comp.Faces)
		comps = append(comps, comp)
	}
	sort.Slice(comps, func(i, j int) bool {
		if len(comps[i].Faces) != len(comps[j].Faces) {
			return len(comps[i].Faces) > len(comps[j].Faces)
		}
		return comps[i].Faces[0] < comps[j].Faces[0]
	})
	for i := range comps {
		comps[i].ID = i
	}
	return comps
}

func uniqueInts(a []int) []int {
	if len(a) < 2 {
		return a
	}
	out := a[:1]
	for _, v := range a[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func DuplicateVertexCount(m Mesh) int {
	type key [3]uint64
	mk := func(v Vec3) key {
		return key{math.Float64bits(v.X), math.Float64bits(v.Y), math.Float64bits(v.Z)}
	}
	seen := make(map[key]bool, len(m.Vertices))
	var dup int
	for _, v := range m.Vertices {
		k := mk(v)
		if seen[k] {
			dup++
		} else {
			seen[k] = true
		}
	}
	return dup
}

func UnusedVertexCount(m Mesh) int {
	used := make([]bool, len(m.Vertices))
	for _, f := range m.Faces {
		for _, idx := range f.Indices() {
			if idx >= 0 && idx < len(used) {
				used[idx] = true
			}
		}
	}
	var unused int
	for _, u := range used {
		if !u {
			unused++
		}
	}
	return unused
}

func ThinSheetComponentCount(m Mesh) int {
	comps := FaceComponents(m, ComponentAllSharedEdges)
	if len(comps) <= 1 {
		return 0
	}
	maxFaces := len(comps[0].Faces)
	var count int
	for _, c := range comps[1:] {
		if len(c.Faces)*20 < maxFaces {
			area := 0.0
			var used []Vec3
			seen := make(map[int]bool)
			for _, fi := range c.Faces {
				area += m.FaceArea(fi)
				f := m.Faces[fi]
				for _, vi := range f.Indices() {
					if !seen[vi] {
						seen[vi] = true
						used = append(used, m.Vertices[vi])
					}
				}
			}
			cb := boundsOf(used)
			d2 := cb.Diagonal * cb.Diagonal
			if area < 0.35*d2 {
				count++
			}
		}
	}
	return count
}

type bounds struct {
	Min, Max Vec3
	Diagonal float64
}

func boundsOf(vs []Vec3) bounds {
	if len(vs) == 0 {
		return bounds{}
	}
	b := bounds{Min: vs[0], Max: vs[0]}
	for _, v := range vs[1:] {
		if v.X < b.Min.X {
			b.Min.X = v.X
		}
		if v.Y < b.Min.Y {
			b.Min.Y = v.Y
		}
		if v.Z < b.Min.Z {
			b.Min.Z = v.Z
		}
		if v.X > b.Max.X {
			b.Max.X = v.X
		}
		if v.Y > b.Max.Y {
			b.Max.Y = v.Y
		}
		if v.Z > b.Max.Z {
			b.Max.Z = v.Z
		}
	}
	b.Diagonal = b.Max.Sub(b.Min).Len()
	return b
}
