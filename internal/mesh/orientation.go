package mesh

import (
	"math"
	"sort"
)

type OrientationReport struct {
	FaceFlips              []bool    `json:"face_flips"`
	FlippedFaces           int       `json:"flipped_faces"`
	ConflictEdges          int       `json:"conflict_edges"`
	ConflictComponents     []int     `json:"conflict_components"`
	Components             int       `json:"components"`
	ComponentSignedVolumes []float64 `json:"component_signed_volumes"`
}

type orientationAdj struct {
	face int
	flip bool
}

// AnalyzeOrientation solves a parity constraint per edge-connected component,
// then resolves the remaining global sign by making closed volume positive.
// Non-manifold edges are intentionally not used as propagation constraints.
func AnalyzeOrientation(m Mesh) OrientationReport {
	t := BuildTopology(m)
	adj := make([][]orientationAdj, len(m.Faces))
	conflictEdges := 0
	for _, e := range t.SortedEdges() {
		if len(e.Uses) != 2 {
			continue
		}
		u, v := e.Uses[0], e.Uses[1]
		same := u.From == v.From
		if same {
			conflictEdges++
		}
		adj[u.Face] = append(adj[u.Face], orientationAdj{face: v.Face, flip: same})
		adj[v.Face] = append(adj[v.Face], orientationAdj{face: u.Face, flip: same})
	}
	for i := range adj {
		sort.Slice(adj[i], func(a, b int) bool { return adj[i][a].face < adj[i][b].face })
	}

	flips := make([]bool, len(m.Faces))
	visited := make([]int, len(m.Faces))
	for i := range visited {
		visited[i] = -1
	}
	comp := 0
	var conflictComps []int
	var globalVolumes []float64
	for root := range m.Faces {
		if visited[root] >= 0 {
			continue
		}
		visited[root] = comp
		queue := []int{root}
		var faces []int
		conflict := false
		for len(queue) > 0 {
			f := queue[0]
			queue = queue[1:]
			faces = append(faces, f)
			for _, nb := range adj[f] {
				want := flips[f] != nb.flip
				if visited[nb.face] < 0 {
					visited[nb.face] = comp
					flips[nb.face] = want
					queue = append(queue, nb.face)
				} else if flips[nb.face] != want {
					conflict = true
				}
			}
		}
		sort.Ints(faces)
		vol := componentVolume(m, faces, flips)
		globalVolumes = append(globalVolumes, vol)
		significant := len(faces)*20 >= len(m.Faces)
		if math.Abs(vol) > 1e-12 && vol < 0 && (significant || componentClosed(m, t, faces)) {
			for _, f := range faces {
				flips[f] = !flips[f]
			}
		}
		if conflict {
			conflictComps = append(conflictComps, comp)
		}
		comp++
	}
	flipped := 0
	for _, f := range flips {
		if f {
			flipped++
		}
	}
	return OrientationReport{
		FaceFlips:              flips,
		FlippedFaces:           flipped,
		ConflictEdges:          conflictEdges,
		ConflictComponents:     conflictComps,
		Components:             comp,
		ComponentSignedVolumes: globalVolumes,
	}
}

func componentClosed(m Mesh, t Topology, faces []int) bool {
	for _, fi := range faces {
		f := m.Faces[fi]
		for _, e := range [][2]int{{f.A, f.B}, {f.B, f.C}, {f.C, f.A}} {
			if rec := t.Edges[MakeEdge(e[0], e[1])]; rec == nil || len(rec.Uses) != 2 {
				return false
			}
		}
	}
	return true
}

func componentVolume(m Mesh, faces []int, flips []bool) float64 {
	var volume float64
	for _, fi := range faces {
		f := m.Faces[fi]
		a, b, c := m.Vertices[f.A], m.Vertices[f.B], m.Vertices[f.C]
		if flips[fi] {
			b, c = c, b
		}
		volume += a.Dot(b.Cross(c)) / 6
	}
	return volume
}

func ApplyOrientation(m Mesh, report OrientationReport) Mesh {
	out := m.Clone()
	for i, flip := range report.FaceFlips {
		if flip {
			out.Faces[i] = out.Faces[i].Reversed()
		}
	}
	return out
}
