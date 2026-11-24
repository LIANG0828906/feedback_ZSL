package model

import (
	"container/heap"
	"sort"
)

// Graph is the immutable production dependency graph.
type Graph struct {
	Order     []string            `json:"order"`
	Edges     map[string][]string `json:"edges"`
	Reverse   map[string][]string `json:"reverse"`
	nodeIndex map[string]int
}

// BuildGraph constructs producer-to-consumer edges and a deterministic
// topological order. Declaration order breaks ties.
func BuildGraph(m *Model) (*Graph, error) {
	nodeIndex := make(map[string]int, len(m.Nodes))
	for i, node := range m.Nodes {
		nodeIndex[node.ID] = i
	}
	edges := make(map[string][]string, len(m.Nodes))
	reverse := make(map[string][]string, len(m.Nodes))
	edgeSeen := make(map[[2]string]bool)
	producers := make(map[string][]string)
	for _, node := range m.Nodes {
		recipe, ok := node.RecipeByID(node.Recipe)
		if !ok {
			continue
		}
		for resource := range recipe.Outputs {
			producers[resource] = append(producers[resource], node.ID)
		}
	}
	for _, node := range m.Nodes {
		recipe, ok := node.RecipeByID(node.Recipe)
		if !ok {
			continue
		}
		for resource := range recipe.Inputs {
			for _, producer := range producers[resource] {
				if producer == node.ID {
					continue
				}
				edge := [2]string{producer, node.ID}
				if edgeSeen[edge] {
					continue
				}
				edgeSeen[edge] = true
				edges[producer] = append(edges[producer], node.ID)
				reverse[node.ID] = append(reverse[node.ID], producer)
			}
		}
	}
	for id := range edges {
		sort.Slice(edges[id], func(i, j int) bool {
			return nodeIndex[edges[id][i]] < nodeIndex[edges[id][j]]
		})
	}
	for id := range reverse {
		sort.Slice(reverse[id], func(i, j int) bool {
			return nodeIndex[reverse[id][i]] < nodeIndex[reverse[id][j]]
		})
	}

	indegree := make(map[string]int, len(m.Nodes))
	for _, node := range m.Nodes {
		indegree[node.ID] = 0
	}
	for _, targets := range edges {
		for _, target := range targets {
			indegree[target]++
		}
	}
	queue := &indexHeap{index: nodeIndex}
	for _, node := range m.Nodes {
		if indegree[node.ID] == 0 {
			heap.Push(queue, node.ID)
		}
	}
	order := make([]string, 0, len(m.Nodes))
	for queue.Len() > 0 {
		id := heap.Pop(queue).(string)
		order = append(order, id)
		for _, target := range edges[id] {
			indegree[target]--
			if indegree[target] == 0 {
				heap.Push(queue, target)
			}
		}
	}
	if len(order) != len(m.Nodes) {
		return nil, NewError(ESchema, "/nodes", "production dependency graph contains a cycle")
	}
	return &Graph{
		Order:     order,
		Edges:     edges,
		Reverse:   reverse,
		nodeIndex: nodeIndex,
	}, nil
}

// Downstream returns the changed node plus all transitive consumers in
// topological order.
func (g *Graph) Downstream(nodeID string) []string {
	seen := map[string]bool{nodeID: true}
	queue := []string{nodeID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range g.Edges[current] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, id := range g.Order {
		if seen[id] {
			out = append(out, id)
		}
	}
	return out
}

// UpdateNodeRecipe updates only the edges incident to one node after its
// selected recipe changes. Existing edges between other nodes are retained.
func (g *Graph) UpdateNodeRecipe(m *Model, nodeID string) error {
	node := m.NodeMap()[nodeID]
	if node == nil {
		return NewError(EUnit, "/nodes/"+nodeID, "node does not exist")
	}
	nodeIndex := make(map[string]int, len(m.Nodes))
	for i, candidate := range m.Nodes {
		nodeIndex[candidate.ID] = i
	}
	for source, targets := range g.Edges {
		filtered := targets[:0]
		for _, target := range targets {
			if source != nodeID && target != nodeID {
				filtered = append(filtered, target)
			}
		}
		if len(filtered) == 0 {
			delete(g.Edges, source)
		} else {
			g.Edges[source] = filtered
		}
	}
	for target, sources := range g.Reverse {
		filtered := sources[:0]
		for _, source := range sources {
			if source != nodeID && target != nodeID {
				filtered = append(filtered, source)
			}
		}
		if len(filtered) == 0 {
			delete(g.Reverse, target)
		} else {
			g.Reverse[target] = filtered
		}
	}
	producers := make(map[string][]string)
	for _, candidate := range m.Nodes {
		recipe, ok := candidate.RecipeByID(candidate.Recipe)
		if !ok {
			continue
		}
		for resource := range recipe.Outputs {
			producers[resource] = append(producers[resource], candidate.ID)
		}
	}
	recipe, ok := node.RecipeByID(node.Recipe)
	if !ok {
		return NewError(ESchema, "/nodes/"+nodeID+"/recipe", "selected recipe does not exist")
	}
	edgeSeen := make(map[[2]string]bool)
	for source, targets := range g.Edges {
		for _, target := range targets {
			edgeSeen[[2]string{source, target}] = true
		}
	}
	addEdge := func(source, target string) {
		pair := [2]string{source, target}
		if source == target || edgeSeen[pair] {
			return
		}
		edgeSeen[pair] = true
		g.Edges[source] = append(g.Edges[source], target)
		g.Reverse[target] = append(g.Reverse[target], source)
	}
	for resource := range recipe.Inputs {
		for _, producer := range producers[resource] {
			addEdge(producer, nodeID)
		}
	}
	for resource := range recipe.Outputs {
		for _, candidate := range m.Nodes {
			if candidate.ID == nodeID {
				continue
			}
			candidateRecipe, ok := candidate.RecipeByID(candidate.Recipe)
			if !ok {
				continue
			}
			if _, consumes := candidateRecipe.Inputs[resource]; consumes {
				addEdge(nodeID, candidate.ID)
			}
		}
	}
	for id := range g.Edges {
		sort.Slice(g.Edges[id], func(i, j int) bool {
			return nodeIndex[g.Edges[id][i]] < nodeIndex[g.Edges[id][j]]
		})
	}
	for id := range g.Reverse {
		sort.Slice(g.Reverse[id], func(i, j int) bool {
			return nodeIndex[g.Reverse[id][i]] < nodeIndex[g.Reverse[id][j]]
		})
	}
	order, err := topoOrder(m, g.Edges, nodeIndex)
	if err != nil {
		return err
	}
	g.Order = order
	return nil
}

func topoOrder(m *Model, edges map[string][]string, nodeIndex map[string]int) ([]string, error) {
	indegree := make(map[string]int, len(m.Nodes))
	for _, node := range m.Nodes {
		indegree[node.ID] = 0
	}
	for _, targets := range edges {
		for _, target := range targets {
			indegree[target]++
		}
	}
	queue := &indexHeap{index: nodeIndex}
	for _, node := range m.Nodes {
		if indegree[node.ID] == 0 {
			heap.Push(queue, node.ID)
		}
	}
	order := make([]string, 0, len(m.Nodes))
	for queue.Len() > 0 {
		id := heap.Pop(queue).(string)
		order = append(order, id)
		for _, target := range edges[id] {
			indegree[target]--
			if indegree[target] == 0 {
				heap.Push(queue, target)
			}
		}
	}
	if len(order) != len(m.Nodes) {
		return nil, NewError(ESchema, "/nodes", "production dependency graph contains a cycle")
	}
	return order, nil
}

type indexHeap struct {
	items []string
	index map[string]int
}

func (h indexHeap) Len() int           { return len(h.items) }
func (h indexHeap) Less(i, j int) bool { return h.index[h.items[i]] < h.index[h.items[j]] }
func (h indexHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *indexHeap) Push(x any)        { h.items = append(h.items, x.(string)) }
func (h *indexHeap) Pop() any {
	old := h.items
	last := old[len(old)-1]
	h.items = old[:len(old)-1]
	return last
}
