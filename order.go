package acousticfield

import (
	"container/heap"
	"sort"
)

// BoundaryTopologicalOrder returns a deterministic topological order of the
// strongly connected components of the boundary graph, with chunk IDs sorted
// inside each component. It is the commit order used by the engine.
func BoundaryTopologicalOrder(level *Level) []int {
	nb := gridNeighbors(level)
	ids := chunkIDsSorted(level)
	adj := make(map[int][]int, len(ids))
	for _, id := range ids {
		sides := make([]string, 0, len(nb[id]))
		for side := range nb[id] {
			sides = append(sides, side)
		}
		sort.Strings(sides)
		for _, side := range sides {
			adj[id] = append(adj[id], nb[id][side])
		}
		sort.Ints(adj[id])
	}

	index := 0
	indices := make(map[int]int, len(ids))
	low := make(map[int]int, len(ids))
	onStack := make(map[int]bool, len(ids))
	var stack []int
	comp := make(map[int]int, len(ids))
	compMin := []int{}
	var strongConnect func(v int)
	strongConnect = func(v int) {
		indices[v] = index
		low[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if _, seen := indices[w]; !seen {
				strongConnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] {
				if indices[w] < low[v] {
					low[v] = indices[w]
				}
			}
		}
		if low[v] == indices[v] {
			cid := len(compMin)
			minID := v
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp[w] = cid
				if w < minID {
					minID = w
				}
				if w == v {
					break
				}
			}
			compMin = append(compMin, minID)
		}
	}
	for _, id := range ids {
		if _, seen := indices[id]; !seen {
			strongConnect(id)
		}
	}

	compAdj := make(map[int]map[int]struct{})
	indeg := make(map[int]int)
	for cid := range compMin {
		indeg[cid] = 0
	}
	for _, v := range ids {
		for _, w := range adj[v] {
			a, b := comp[v], comp[w]
			if a == b {
				continue
			}
			if compAdj[a] == nil {
				compAdj[a] = map[int]struct{}{}
			}
			if _, exists := compAdj[a][b]; !exists {
				compAdj[a][b] = struct{}{}
				indeg[b]++
			}
		}
	}
	pq := &compHeap{minIDs: compMin}
	for cid, d := range indeg {
		if d == 0 {
			heap.Push(pq, cid)
		}
	}
	var orderedComps []int
	for pq.Len() > 0 {
		cid := heap.Pop(pq).(int)
		orderedComps = append(orderedComps, cid)
		next := make([]int, 0, len(compAdj[cid]))
		for to := range compAdj[cid] {
			next = append(next, to)
		}
		sort.Ints(next)
		for _, to := range next {
			indeg[to]--
			if indeg[to] == 0 {
				heap.Push(pq, to)
			}
		}
	}
	var out []int
	for _, cid := range orderedComps {
		var members []int
		for _, id := range ids {
			if comp[id] == cid {
				members = append(members, id)
			}
		}
		sort.Ints(members)
		out = append(out, members...)
	}
	return out
}

type compHeap struct {
	minIDs []int
	items  []int
}

func (h compHeap) Len() int { return len(h.items) }
func (h compHeap) Less(i, j int) bool {
	return h.minIDs[h.items[i]] < h.minIDs[h.items[j]]
}
func (h compHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *compHeap) Push(x any)   { h.items = append(h.items, x.(int)) }
func (h *compHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}
