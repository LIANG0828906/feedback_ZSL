package segment

import (
	"fmt"
	"math"
	"sort"

	"iconbench/internal/model"
)

type MergeOutput struct {
	Labels   []int32
	Regions  []RegionInfo
	Edges    []RAGEdge
	MergeLog []MergeEvent
	Error    error
}

type dsu struct{ parent []int32 }

func newDSU(n int32) *dsu {
	d := &dsu{parent: make([]int32, n+1)}
	for i := range d.parent {
		d.parent[i] = int32(i)
	}
	return d
}

func (d *dsu) find(x int32) int32 {
	if d.parent[x] == x {
		return x
	}
	d.parent[x] = d.find(d.parent[x])
	return d.parent[x]
}

func (d *dsu) union(a, b int32) int32 {
	ra, rb := d.find(a), d.find(b)
	if ra == rb {
		return ra
	}
	if ra < rb {
		d.parent[rb] = ra
		return ra
	}
	d.parent[ra] = rb
	return rb
}

func BuildRAG(labels []int32, gradient []float64, w, h int, classes map[int32]uint8, areas map[int32]int, p model.Params) []RAGEdge {
	type acc struct {
		edge RAGEdge
	}
	edgeMap := map[uint64]*acc{}
	add := func(a, b int32, g float64) {
		if a <= 0 || b <= 0 || a == b {
			return
		}
		lo, hi := a, b
		if lo > hi {
			lo, hi = hi, lo
		}
		key := uint64(uint32(lo))<<32 | uint64(uint32(hi))
		item := edgeMap[key]
		if item == nil {
			diff := int(math.Abs(float64(areas[lo] - areas[hi])))
			areaCost := 100 * float64(diff) / float64(w*h)
			cost := math.Min(255, math.Max(0, g)) + areaCost
			item = &acc{edge: RAGEdge{A: lo, B: hi, MinGradient: g, AreaA: areas[lo], AreaB: areas[hi], AreaDifference: diff, Cost: cost, BoundaryPixels: 1}}
			edgeMap[key] = item
		} else {
			if g < item.edge.MinGradient {
				item.edge.MinGradient = g
			}
			item.edge.BoundaryPixels++
		}
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			a := labels[i]
			if x+1 < w {
				b := labels[i+1]
				if a > 0 && b > 0 && a != b {
					add(a, b, math.Min(gradient[i], gradient[i+1]))
				}
			}
			if y+1 < h {
				b := labels[i+w]
				if a > 0 && b > 0 && a != b {
					add(a, b, math.Min(gradient[i], gradient[i+w]))
				}
			}
			if a == 0 {
				neighbors := uniquePositive(neighbor4Indices(x, y, w, h), labels)
				for ai := 0; ai < len(neighbors); ai++ {
					for bi := ai + 1; bi < len(neighbors); bi++ {
						add(neighbors[ai], neighbors[bi], gradient[i])
					}
				}
			}
		}
	}

	edges := make([]RAGEdge, 0, len(edgeMap))
	for _, item := range edgeMap {
		e := item.edge
		classSame := classes[e.A] == classes[e.B]
		e.Feasible = classSame && e.AreaDifference <= p.AreaDiffCap && e.Cost <= p.MergeCostCap
		switch {
		case !classSame:
			e.Reason = "seed_class_mismatch"
		case e.AreaDifference > p.AreaDiffCap:
			e.Reason = "area_difference_cap"
		case e.Cost > p.MergeCostCap:
			e.Reason = "cost_cap"
		default:
			e.Reason = "feasible"
		}
		edges = append(edges, e)
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].Cost != edges[j].Cost {
			return edges[i].Cost < edges[j].Cost
		}
		if edges[i].A != edges[j].A {
			return edges[i].A < edges[j].A
		}
		return edges[i].B < edges[j].B
	})
	return edges
}

func uniquePositive(indices []int, labels []int32) []int32 {
	seen := map[int32]bool{}
	var out []int32
	for _, idx := range indices {
		v := labels[idx]
		if v > 0 && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func MergeRegions(labels []int32, gradient []float64, mr MarkerResult, w, h int, p model.Params, maxIterations int) (*MergeOutput, error) {
	classes := map[int32]uint8{}
	var maxLabel int32
	for _, m := range mr.Markers {
		classes[m.ID] = m.Class
		if m.ID > maxLabel {
			maxLabel = m.ID
		}
	}
	areas := map[int32]int{}
	for _, l := range labels {
		if l > 0 {
			areas[l]++
		}
	}
	edges := BuildRAG(labels, gradient, w, h, classes, areas, p)
	dsu := newDSU(maxLabel)
	var log []MergeEvent

	// Enforce minimum atomic region size with deterministic cheapest same-class merge.
	atomicIDs := make([]int32, 0, len(areas))
	for id := range areas {
		atomicIDs = append(atomicIDs, id)
	}
	sort.Slice(atomicIDs, func(i, j int) bool { return atomicIDs[i] < atomicIDs[j] })
	for _, id := range atomicIDs {
		if areas[id] >= p.MinRegionArea {
			continue
		}
		best := -1
		for i := range edges {
			e := &edges[i]
			if classes[e.A] != classes[e.B] {
				continue
			}
			if (e.A == id || e.B == id) && (best < 0 || e.Cost < edges[best].Cost || (e.Cost == edges[best].Cost && edgeLess(e, &edges[best]))) {
				best = i
			}
		}
		if best >= 0 {
			e := edges[best]
			ra, rb := dsu.find(e.A), dsu.find(e.B)
			if ra != rb {
				root := dsu.union(e.A, e.B)
				log = append(log, MergeEvent{Step: len(log) + 1, A: e.A, B: e.B, RootA: ra, RootB: rb, Cost: e.Cost, Reason: "min_region_area"})
				_ = root
			}
		}
	}

	if maxIterations <= 0 {
		maxIterations = w*h + 1
	}
	for _, e := range edges {
		if e.Cost > p.MergeCostCap {
			break
		}
		if classes[e.A] != classes[e.B] || e.AreaDifference > p.AreaDiffCap {
			continue
		}
		ra, rb := dsu.find(e.A), dsu.find(e.B)
		if ra == rb {
			continue
		}
		if len(log) >= maxIterations {
			remaining, minCost := remainingMerges(edges, dsu, classes, p)
			err := &model.Error{
				Code:           model.CodeMerge,
				Message:        fmt.Sprintf("merge did not terminate within %d iterations", maxIterations),
				RemainingPairs: remaining,
				MinCost:        minCost,
			}
			return &MergeOutput{Edges: edges, MergeLog: log}, err
		}
		root := dsu.union(e.A, e.B)
		log = append(log, MergeEvent{Step: len(log) + 1, A: e.A, B: e.B, RootA: ra, RootB: rb, Cost: e.Cost, Reason: "cost_cap"})
		_ = root
	}

	out := make([]int32, len(labels))
	rootAreas := map[int32]int{}
	rootSeeds := map[int32][]int32{}
	for i, l := range labels {
		if l <= 0 {
			continue
		}
		r := dsu.find(l)
		out[i] = r
		rootAreas[r]++
		if !containsInt32(rootSeeds[r], l) {
			rootSeeds[r] = append(rootSeeds[r], l)
		}
	}
	var regions []RegionInfo
	roots := make([]int32, 0, len(rootAreas))
	for r := range rootAreas {
		roots = append(roots, r)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] < roots[j] })
	for _, r := range roots {
		class := uint8(0)
		seeds := rootSeeds[r]
		sort.Slice(seeds, func(i, j int) bool { return seeds[i] < seeds[j] })
		if len(seeds) > 0 {
			class = classes[seeds[0]]
		}
		regions = append(regions, RegionInfo{Label: r, Class: class, Area: rootAreas[r], SeedIDs: seeds})
	}

	return &MergeOutput{Labels: out, Regions: regions, Edges: edges, MergeLog: log}, nil
}

func edgeLess(a, b *RAGEdge) bool {
	if a.Cost != b.Cost {
		return a.Cost < b.Cost
	}
	if a.A != b.A {
		return a.A < b.A
	}
	return a.B < b.B
}

func remainingMerges(edges []RAGEdge, dsu *dsu, classes map[int32]uint8, p model.Params) (int, float64) {
	count := 0
	minCost := math.Inf(1)
	for _, e := range edges {
		if e.Cost > p.MergeCostCap || e.AreaDifference > p.AreaDiffCap || classes[e.A] != classes[e.B] {
			continue
		}
		if dsu.find(e.A) == dsu.find(e.B) {
			continue
		}
		count++
		if e.Cost < minCost {
			minCost = e.Cost
		}
	}
	if count == 0 {
		return 0, 0
	}
	return count, minCost
}

func containsInt32(xs []int32, v int32) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
