package dependency

import (
	"fmt"
	"sort"

	"terrainnormalverify/internal/model"
)

type Graph struct {
	Neighbors    map[int][]int    `json:"neighbors"`
	LayerBlocks  map[string][]int `json:"layerBlocks"`
	SeamsByBlock map[int][]string `json:"seamsByBlock"`
	BlockLayers  map[int][]string `json:"blockLayers"`
}

type InvalidationEntry struct {
	BlockID      int      `json:"blockId"`
	Invalid      bool     `json:"invalid"`
	Reused       bool     `json:"reused"`
	Reason       string   `json:"reason"`
	Dependencies []int    `json:"dependencies"`
	Layers       []string `json:"layers"`
}

func Build(project *model.Project) *Graph {
	g := &Graph{
		Neighbors:    make(map[int][]int),
		LayerBlocks:  make(map[string][]int),
		SeamsByBlock: make(map[int][]string),
		BlockLayers:  make(map[int][]string),
	}
	for _, block := range project.Blocks {
		g.Neighbors[block.ID] = nil
		for i, layer := range project.Layers {
			for _, row := range block.Weights {
				if i < len(row) && row[i] > 0 {
					g.BlockLayers[block.ID] = append(g.BlockLayers[block.ID], layer.ID)
					g.LayerBlocks[layer.ID] = appendUnique(g.LayerBlocks[layer.ID], block.ID)
					break
				}
			}
		}
	}
	for _, seam := range project.Seams {
		g.Neighbors[seam.BlockA] = appendUnique(g.Neighbors[seam.BlockA], seam.BlockB)
		g.Neighbors[seam.BlockB] = appendUnique(g.Neighbors[seam.BlockB], seam.BlockA)
		g.SeamsByBlock[seam.BlockA] = append(g.SeamsByBlock[seam.BlockA], seam.ID)
		g.SeamsByBlock[seam.BlockB] = append(g.SeamsByBlock[seam.BlockB], seam.ID)
	}
	for id := range g.Neighbors {
		sort.Ints(g.Neighbors[id])
	}
	for id := range g.SeamsByBlock {
		sort.Strings(g.SeamsByBlock[id])
	}
	for id := range g.BlockLayers {
		sort.Strings(g.BlockLayers[id])
	}
	for id := range g.LayerBlocks {
		sort.Ints(g.LayerBlocks[id])
	}
	return g
}

func (g *Graph) InvalidateHeight(blockID int, reason string) map[int]string {
	return g.closure([]int{blockID}, reason)
}

func (g *Graph) InvalidateLayer(project *model.Project, layerID, reason string) map[int]string {
	var seeds []int
	for _, block := range project.SortedBlocks() {
		for i, layer := range project.Layers {
			if layer.ID != layerID {
				continue
			}
			for _, row := range block.Weights {
				if i < len(row) && row[i] > 0 {
					seeds = append(seeds, block.ID)
					break
				}
			}
		}
	}
	return g.closure(seeds, reason)
}

func (g *Graph) InvalidateAll(project *model.Project, reason string) map[int]string {
	out := make(map[int]string, len(project.Blocks))
	for _, block := range project.Blocks {
		out[block.ID] = reason
	}
	return out
}

func (g *Graph) closure(seeds []int, reason string) map[int]string {
	out := make(map[int]string)
	queue := append([]int(nil), seeds...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if _, seen := out[id]; seen {
			continue
		}
		out[id] = reason
		queue = append(queue, g.Neighbors[id]...)
	}
	return out
}

func (g *Graph) TopologicalOrder(affected map[int]string) []int {
	nodes := make([]int, 0, len(affected))
	for id := range affected {
		nodes = append(nodes, id)
	}
	sort.Ints(nodes)
	index := 0
	indices := make(map[int]int, len(nodes))
	lowlink := make(map[int]int, len(nodes))
	onStack := make(map[int]bool, len(nodes))
	var stack []int
	var components [][]int
	var visit func(int)
	visit = func(v int) {
		indices[v] = index
		lowlink[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range g.Neighbors[v] {
			if _, ok := affected[w]; !ok {
				continue
			}
			if _, seen := indices[w]; !seen {
				visit(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] && indices[w] < lowlink[v] {
				lowlink[v] = indices[w]
			}
		}
		if lowlink[v] != indices[v] {
			return
		}
		var component []int
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			component = append(component, w)
			if w == v {
				break
			}
		}
		sort.Ints(component)
		components = append(components, component)
	}
	for _, node := range nodes {
		if _, seen := indices[node]; !seen {
			visit(node)
		}
	}
	sort.Slice(components, func(i, j int) bool { return components[i][0] < components[j][0] })
	componentOf := make(map[int]int)
	for i, component := range components {
		for _, id := range component {
			componentOf[id] = i
		}
	}
	indegree := make([]int, len(components))
	edgeSets := make([]map[int]bool, len(components))
	for i := range edgeSets {
		edgeSets[i] = make(map[int]bool)
	}
	for id := range affected {
		from := componentOf[id]
		for _, neighbor := range g.Neighbors[id] {
			if _, ok := affected[neighbor]; !ok {
				continue
			}
			to := componentOf[neighbor]
			if from != to {
				edgeSets[from][to] = true
			}
		}
	}
	edges := make([][]int, len(components))
	for from, set := range edgeSets {
		for to := range set {
			edges[from] = append(edges[from], to)
			indegree[to]++
		}
		sort.Ints(edges[from])
	}
	var queue []int
	for i := range components {
		if indegree[i] == 0 {
			queue = append(queue, i)
		}
	}
	sort.Ints(queue)
	var order []int
	for len(queue) > 0 {
		component := queue[0]
		queue = queue[1:]
		order = append(order, components[component]...)
		for _, next := range edges[component] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
				sort.Ints(queue)
			}
		}
	}
	if len(order) != len(nodes) {
		order = nodes
	}
	return order
}

func (g *Graph) Manifest(project *model.Project, affected map[int]string, baked map[int]bool) []InvalidationEntry {
	entries := make([]InvalidationEntry, 0, len(project.Blocks))
	for _, block := range project.SortedBlocks() {
		reason, invalid := affected[block.ID]
		entry := InvalidationEntry{
			BlockID:      block.ID,
			Invalid:      invalid,
			Reused:       !invalid,
			Reason:       reason,
			Dependencies: append([]int(nil), g.Neighbors[block.ID]...),
			Layers:       append([]string(nil), g.BlockLayers[block.ID]...),
		}
		if invalid {
			entry.Reused = !baked[block.ID]
		}
		if !invalid {
			entry.Reason = "cache hit: input hash and dependency set unchanged"
		}
		entries = append(entries, entry)
	}
	return entries
}

func (g *Graph) DependencySet(blockID int) map[string]any {
	return map[string]any{
		"blocks": append([]int(nil), g.Neighbors[blockID]...),
		"layers": append([]string(nil), g.BlockLayers[blockID]...),
		"seams":  append([]string(nil), g.SeamsByBlock[blockID]...),
	}
}

func (g *Graph) Describe(blockID int) string {
	return fmt.Sprintf("blocks=%v layers=%v seams=%v", g.Neighbors[blockID], g.BlockLayers[blockID], g.SeamsByBlock[blockID])
}

func appendUnique(values []int, value int) []int {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
