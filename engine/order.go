package engine

import (
	"container/heap"
	"sort"

	"economycore/model"
)

// runtimeOrder builds the current cycle's topological ordering from the
// recipes selected in runtime state. This keeps recipe-switch intents correct
// without mutating or rebuilding the persistent invalidation graph.
func (e *Engine) runtimeOrder(window int) ([]string, error) {
	index := make(map[string]int, len(e.model.Nodes))
	for i, node := range e.model.Nodes {
		index[node.ID] = i
	}
	producers := make(map[string][]string)
	for _, node := range e.model.Nodes {
		rt := e.state.Nodes[node.ID]
		if !rt.Built {
			continue
		}
		recipe, ok := node.RecipeByID(rt.Recipe)
		if !ok {
			return nil, model.NewError(model.EUnit, "/nodes/"+node.ID+"/recipe", "selected runtime recipe does not exist").WithWindow(window).WithNode(node.ID, "recipe")
		}
		for resource := range recipe.Outputs {
			producers[resource] = append(producers[resource], node.ID)
		}
	}
	edges := make(map[string][]string, len(e.model.Nodes))
	indegree := make(map[string]int, len(e.model.Nodes))
	for _, node := range e.model.Nodes {
		indegree[node.ID] = 0
	}
	for _, node := range e.model.Nodes {
		rt := e.state.Nodes[node.ID]
		if !rt.Built {
			continue
		}
		recipe, _ := node.RecipeByID(rt.Recipe)
		seen := make(map[string]bool)
		for resource := range recipe.Inputs {
			for _, producer := range producers[resource] {
				if producer == node.ID || seen[producer] {
					continue
				}
				seen[producer] = true
				edges[producer] = append(edges[producer], node.ID)
				indegree[node.ID]++
			}
		}
	}
	for id := range edges {
		sort.Slice(edges[id], func(i, j int) bool {
			return index[edges[id][i]] < index[edges[id][j]]
		})
	}
	queue := &nodeHeap{index: index}
	for _, node := range e.model.Nodes {
		if e.state.Nodes[node.ID].Built && indegree[node.ID] == 0 {
			heap.Push(queue, node.ID)
		}
	}
	order := make([]string, 0, len(e.model.Nodes))
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
	builtCount := 0
	for _, node := range e.model.Nodes {
		if e.state.Nodes[node.ID].Built {
			builtCount++
		}
	}
	if len(order) != builtCount {
		return nil, model.NewError(model.ESchema, "/nodes", "runtime recipe dependency graph contains a cycle").WithWindow(window)
	}
	for _, node := range e.model.Nodes {
		if !e.state.Nodes[node.ID].Built {
			order = append(order, node.ID)
		}
	}
	return order, nil
}

type nodeHeap struct {
	items []string
	index map[string]int
}

func (h nodeHeap) Len() int           { return len(h.items) }
func (h nodeHeap) Less(i, j int) bool { return h.index[h.items[i]] < h.index[h.items[j]] }
func (h nodeHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *nodeHeap) Push(value any)    { h.items = append(h.items, value.(string)) }
func (h *nodeHeap) Pop() any {
	old := h.items
	last := old[len(old)-1]
	h.items = old[:len(old)-1]
	return last
}
