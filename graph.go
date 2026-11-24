package terrain

import (
	"container/heap"
	"fmt"
)

type DependencyGraph struct {
	Dependencies []Dependency              `json:"dependencies"`
	Adjacency    map[int][]Dependency      `json:"adjacency"`
	Boundaries   map[string]SharedBoundary `json:"-"`
}

func BuildDependencyGraph(p *Project) *DependencyGraph {
	g := &DependencyGraph{Adjacency: map[int][]Dependency{}, Boundaries: map[string]SharedBoundary{}}
	for _, b := range p.SharedBoundaries {
		g.Boundaries[b.ID] = b
		g.Dependencies = append(g.Dependencies,
			Dependency{BoundaryID: b.ID, BlockNo: b.BlockA, DependsOn: b.BlockB, Direction: "bidirectional"},
			Dependency{BoundaryID: b.ID, BlockNo: b.BlockB, DependsOn: b.BlockA, Direction: "bidirectional"},
		)
		g.Adjacency[b.BlockA] = append(g.Adjacency[b.BlockA], g.Dependencies[len(g.Dependencies)-2])
		g.Adjacency[b.BlockB] = append(g.Adjacency[b.BlockB], g.Dependencies[len(g.Dependencies)-1])
	}
	return g
}

type BlockChange struct {
	BlockNo int    `json:"block_no"`
	Kind    string `json:"kind"`
}

type InvalidationPlan struct {
	Items []InvalidationItem `json:"items"`
	Order []int              `json:"topological_order"`
}

func PlanInvalidation(p *Project, changes []BlockChange) (*InvalidationPlan, error) {
	graph := BuildDependencyGraph(p)
	reasons := map[int]string{}
	boundaryByBlock := map[int][]string{}
	for _, ch := range changes {
		if ch.BlockNo < 0 || ch.BlockNo >= len(p.Blocks) {
			bn := ch.BlockNo
			return nil, NewSchemaError("changes[].block_no", &bn, "block number outside project")
		}
		if existing, ok := reasons[ch.BlockNo]; ok && existing != ch.Kind {
			reasons[ch.BlockNo] = "content_or_parameter_changed"
		} else {
			reasons[ch.BlockNo] = ch.Kind
		}
		for _, dep := range graph.Adjacency[ch.BlockNo] {
			neighbor := dep.DependsOn
			boundaryByBlock[ch.BlockNo] = appendUnique(boundaryByBlock[ch.BlockNo], dep.BoundaryID)
			boundaryByBlock[neighbor] = appendUnique(boundaryByBlock[neighbor], dep.BoundaryID)
			if _, changed := reasons[neighbor]; !changed {
				reasons[neighbor] = ReasonBoundary
			} else if reasons[neighbor] != ReasonBoundary {
				reasons[neighbor] = "content_or_parameter_changed"
			}
		}
	}

	plan := &InvalidationPlan{}
	invalidSet := map[int]bool{}
	for i := range p.Blocks {
		reason, invalid := reasons[i]
		if !invalid {
			reason = "unchanged"
		} else {
			invalidSet[i] = true
		}
		item := InvalidationItem{BlockNo: i, Invalid: invalid, Reason: reason, Boundaries: boundaryByBlock[i]}
		if invalid {
			item.Reuse = ReuseRebuild
		} else {
			item.Reuse = ReuseCache
		}
		plan.Items = append(plan.Items, item)
	}
	order, err := TopologicalOrder(p, graph, invalidSet)
	if err != nil {
		return nil, err
	}
	plan.Order = order
	return plan, nil
}

func appendUnique(values []string, v string) []string {
	for _, existing := range values {
		if existing == v {
			return values
		}
	}
	return append(values, v)
}

type intHeap []int

func (h intHeap) Len() int           { return len(h) }
func (h intHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h intHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *intHeap) Push(x any)        { *h = append(*h, x.(int)) }
func (h *intHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// TopologicalOrder turns each bidirectional edge into a stable DAG orientation
// from lower block number to higher block number. The bidirectional dependency
// records remain available separately for propagation and checkpoint metadata.
func TopologicalOrder(p *Project, g *DependencyGraph, only map[int]bool) ([]int, error) {
	indegree := map[int]int{}
	out := map[int][]int{}
	for i := range p.Blocks {
		if len(only) == 0 || only[i] {
			indegree[i] = 0
		}
	}
	seenEdge := map[[2]int]bool{}
	for _, b := range p.SharedBoundaries {
		lo, hi := b.BlockA, b.BlockB
		if lo > hi {
			lo, hi = hi, lo
		}
		if len(only) > 0 && (!only[lo] || !only[hi]) {
			continue
		}
		key := [2]int{lo, hi}
		if seenEdge[key] {
			continue
		}
		seenEdge[key] = true
		if _, ok := indegree[hi]; ok {
			indegree[hi]++
		}
		out[lo] = append(out[lo], hi)
	}
	h := &intHeap{}
	for block, deg := range indegree {
		if deg == 0 {
			heap.Push(h, block)
		}
	}
	var order []int
	for h.Len() > 0 {
		b := heap.Pop(h).(int)
		order = append(order, b)
		for _, next := range out[b] {
			if _, ok := indegree[next]; !ok {
				continue
			}
			indegree[next]--
			if indegree[next] == 0 {
				heap.Push(h, next)
			}
		}
	}
	if len(order) != len(indegree) {
		return nil, fmt.Errorf("dependency graph cycle could not be topologically ordered: got %v", order)
	}
	return order, nil
}
