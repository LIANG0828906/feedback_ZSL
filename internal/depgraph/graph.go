package depgraph

import (
	"sort"

	"terrainpipeline/internal/model"
)

type Node struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

type Edge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type Edit struct {
	Type    string `json:"type"`
	BlockID string `json:"block_id,omitempty"`
	LayerID string `json:"layer_id,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type PlanEntry struct {
	BlockID     string   `json:"block_id"`
	Invalidated bool     `json:"invalidated"`
	WillBake    bool     `json:"will_bake"`
	Reused      bool     `json:"reused"`
	Reasons     []string `json:"reasons"`
}

type Plan struct {
	Edits       []Edit      `json:"edits"`
	Entries     []PlanEntry `json:"entries"`
	Invalidated []string    `json:"invalidated"`
	WillBake    []string    `json:"will_bake"`
	Reused      []string    `json:"reused"`
}

type Graph struct {
	Nodes             []Node              `json:"nodes"`
	Edges             []Edge              `json:"edges"`
	BlockDependencies map[string][]string `json:"block_dependencies"`
	SeamDependencies  map[string][]string `json:"seam_dependencies"`
	LayerUsage        map[string][]string `json:"layer_usage"`
	project           *model.Project
	adjacency         map[string][]Edge
}

func Build(project *model.Project) *Graph {
	g := &Graph{
		BlockDependencies: map[string][]string{},
		SeamDependencies:  map[string][]string{},
		LayerUsage:        map[string][]string{},
		project:           project,
		adjacency:         map[string][]Edge{},
	}

	g.Nodes = append(g.Nodes, Node{"param:differential", "parameter", "差分算子/全局烘焙参数"})
	for _, b := range project.Blocks {
		g.Nodes = append(g.Nodes,
			Node{"source:" + b.ID, "source", "高程 " + b.ID},
			Node{"raw:" + b.ID, "raw", "原始法线 " + b.ID},
			Node{"final:" + b.ID, "final", "最终法线/坡度 " + b.ID},
		)
		g.addEdge("source:"+b.ID, "raw:"+b.ID, "elevation")
		g.addEdge("raw:"+b.ID, "final:"+b.ID, "block-bake")
		g.addEdge("param:differential", "raw:"+b.ID, "bake-parameter")
		for layerIndex, layer := range project.Layers {
			used := false
			for _, weights := range b.Weights {
				if layerIndex < len(weights) && weights[layerIndex] > 0 {
					used = true
					break
				}
			}
			if used {
				g.LayerUsage[layer.ID] = append(g.LayerUsage[layer.ID], b.ID)
				g.addEdge("layer:"+layer.ID, "raw:"+b.ID, "material-layer:"+layer.ID)
			}
		}
	}
	for layerID := range g.LayerUsage {
		g.Nodes = append(g.Nodes, Node{"layer:" + layerID, "parameter", "材质参数 " + layerID})
		sort.Strings(g.LayerUsage[layerID])
	}

	for _, seam := range project.Seams {
		seamNode := "seam:" + seam.ID
		g.Nodes = append(g.Nodes, Node{seamNode, "seam", seam.ID})
		g.addEdge("raw:"+seam.BlockA, seamNode, "seam-input")
		g.addEdge("raw:"+seam.BlockB, seamNode, "seam-input")
		g.addEdge(seamNode, "final:"+seam.BlockA, "seam-weld")
		g.addEdge(seamNode, "final:"+seam.BlockB, "seam-weld")
		g.BlockDependencies[seam.BlockA] = append(g.BlockDependencies[seam.BlockA], seam.BlockB)
		g.BlockDependencies[seam.BlockB] = append(g.BlockDependencies[seam.BlockB], seam.BlockA)
		g.SeamDependencies[seam.ID] = []string{seam.BlockA, seam.BlockB}
	}
	for id := range g.BlockDependencies {
		sort.Strings(g.BlockDependencies[id])
	}
	return g
}

func (g *Graph) addEdge(from, to, reason string) {
	e := Edge{From: from, To: to, Reason: reason}
	g.Edges = append(g.Edges, e)
	g.adjacency[from] = append(g.adjacency[from], e)
}

func (g *Graph) Plan(edits []Edit, cached map[string]bool) *Plan {
	starts := map[string][]string{}
	for _, edit := range edits {
		switch edit.Type {
		case "elevation":
			node := "source:" + edit.BlockID
			starts[node] = append(starts[node], "elevation-edit:"+edit.BlockID)
		case "material":
			node := "layer:" + edit.LayerID
			reason := "material-param:" + edit.LayerID
			if edit.Detail != "" {
				reason += ":" + edit.Detail
			}
			starts[node] = append(starts[node], reason)
		case "operator", "height_scale", "parameter":
			starts["param:differential"] = append(starts["param:differential"], edit.Type+"-change")
		}
	}

	reached := map[string]bool{}
	reachReasons := map[string]map[string]bool{}
	var visit func(node string, reasons []string)
	visit = func(node string, reasons []string) {
		if reached[node] {
			for _, reason := range reasons {
				if reachReasons[node] == nil {
					reachReasons[node] = map[string]bool{}
				}
				reachReasons[node][reason] = true
			}
			return
		}
		reached[node] = true
		if reachReasons[node] == nil {
			reachReasons[node] = map[string]bool{}
		}
		for _, reason := range reasons {
			reachReasons[node][reason] = true
		}
		for _, edge := range g.adjacency[node] {
			next := append(append([]string{}, reasons...), edge.Reason)
			visit(edge.To, next)
		}
	}
	for node, reasons := range starts {
		visit(node, reasons)
	}

	plan := &Plan{Edits: edits}
	for _, block := range g.project.Blocks {
		id := block.ID
		finalNode := "final:" + id
		invalidated := reached[finalNode]
		reasons := sortedReasons(reachReasons[finalNode])
		if invalidated && len(reasons) == 0 {
			reasons = []string{"dependency-reach"}
		}
		hasCache := cached != nil && cached[id]
		entry := PlanEntry{
			BlockID: id, Invalidated: invalidated,
			WillBake: invalidated || !hasCache,
			Reused:   !invalidated && hasCache,
			Reasons:  reasons,
		}
		if !invalidated && !hasCache {
			entry.Reasons = []string{"cold-cache-miss"}
		}
		plan.Entries = append(plan.Entries, entry)
		if invalidated {
			plan.Invalidated = append(plan.Invalidated, id)
		}
		if entry.WillBake {
			plan.WillBake = append(plan.WillBake, id)
		}
		if entry.Reused {
			plan.Reused = append(plan.Reused, id)
		}
	}
	sort.Strings(plan.Invalidated)
	sort.Strings(plan.WillBake)
	sort.Strings(plan.Reused)
	return plan
}

func sortedReasons(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for reason := range set {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

func TopologicalBlockOrder(project *model.Project, graph *Graph, bakeIDs []string) []string {
	needed := map[string]bool{}
	for _, id := range bakeIDs {
		needed[id] = true
	}
	indegree := map[string]int{}
	remainingDeps := map[string]map[string]bool{}
	for _, id := range bakeIDs {
		indegree[id] = 0
		remainingDeps[id] = map[string]bool{}
	}
	for id, neighbors := range graph.BlockDependencies {
		if !needed[id] {
			continue
		}
		for _, neighbor := range neighbors {
			if needed[neighbor] {
				remainingDeps[id][neighbor] = true
				indegree[id]++
			}
		}
	}

	var ready []string
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for other := range remainingDeps {
			if remainingDeps[other][id] {
				delete(remainingDeps[other], id)
				indegree[other]--
				if indegree[other] == 0 {
					ready = append(ready, other)
					sort.Strings(ready)
				}
			}
		}
	}
	if len(order) != len(bakeIDs) {
		return append([]string(nil), bakeIDs...)
	}
	return order
}
