package pipeline

import (
	"sort"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
)

type WeldReport struct {
	Tolerance              float64 `json:"tolerance"`
	ClosePairs             int     `json:"close_pairs"`
	MergedPairs            int     `json:"merged_pairs"`
	RemovedDegenerateFaces int     `json:"removed_degenerate_faces"`
	RemovedDuplicateFaces  int     `json:"removed_duplicate_faces"`
	FirstDegenerateLoop    *int    `json:"first_degenerate_loop,omitempty"`
}

type unionFind struct{ parent []int }

func newUnionFind(n int) *unionFind {
	u := &unionFind{parent: make([]int, n)}
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

func (u *unionFind) union(a, b int) bool {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return false
	}
	if rb < ra {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	return true
}

func Weld(input *mesh.Mesh, tolerance float64) (*mesh.Mesh, WeldReport, error) {
	report := WeldReport{Tolerance: tolerance}
	if tolerance < 0 {
		return nil, report, errs.New(errs.EPARA, "weld tolerance must be non-negative")
	}

	u := newUnionFind(len(input.Vertices))
	for i := 0; i < len(input.Vertices); i++ {
		for j := i + 1; j < len(input.Vertices); j++ {
			if mesh.Distance(input.Vertices[i], input.Vertices[j]) <= tolerance {
				report.ClosePairs++
				if u.union(i, j) {
					report.MergedPairs++
				}
			}
		}
	}

	rootToNew := map[int]int{}
	var vertices []mesh.Vec3
	for i := range input.Vertices {
		root := u.find(i)
		if _, ok := rootToNew[root]; !ok {
			rootToNew[root] = len(vertices)
			vertices = append(vertices, input.Vertices[root])
		}
	}

	out := &mesh.Mesh{Vertices: vertices}
	seenFaces := map[mesh.Face]bool{}
	for _, f := range input.Faces {
		idx := f.Indices()
		mapped := [3]int{}
		for k, old := range idx {
			mapped[k] = rootToNew[u.find(old)]
		}
		nf := mesh.Face{A: mapped[0], B: mapped[1], C: mapped[2]}
		if nf.A == nf.B || nf.B == nf.C || nf.A == nf.C {
			report.RemovedDegenerateFaces++
			continue
		}
		if seenFaces[nf] {
			report.RemovedDuplicateFaces++
			continue
		}
		seenFaces[nf] = true
		out.Faces = append(out.Faces, nf)
	}

	adj := mesh.BuildAdjacency(out)
	for i, loop := range adj.BoundaryLoop {
		unique := map[int]bool{}
		for _, v := range loop.Vertices {
			unique[v] = true
		}
		if !loop.Closed || len(loop.Vertices) < 3 || len(unique) < 3 {
			idx := i
			report.FirstDegenerateLoop = &idx
			err := errs.New(errs.EMERGE,
				"over-large tolerance caused degenerate boundary ring %d; merged_pairs=%d", i, report.ClosePairs)
			return nil, report, errs.WithDetails(err, map[string]any{
				"merged_pairs":          report.ClosePairs,
				"union_merges":          report.MergedPairs,
				"first_degenerate_ring": i,
			})
		}
	}
	if report.RemovedDegenerateFaces > 0 {
		err := errs.New(errs.EMERGE, "weld collapsed %d face(s); merged_pairs=%d", report.RemovedDegenerateFaces, report.ClosePairs)
		details := map[string]any{
			"merged_pairs":             report.ClosePairs,
			"union_merges":             report.MergedPairs,
			"removed_degenerate_faces": report.RemovedDegenerateFaces,
			"first_degenerate_ring":    -1,
		}
		if report.FirstDegenerateLoop != nil {
			details["first_degenerate_ring"] = *report.FirstDegenerateLoop
		}
		return nil, report, errs.WithDetails(err, details)
	}

	return out, report, nil
}

func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
