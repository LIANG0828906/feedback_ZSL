package probe

import (
	"sort"

	"lightprobe/internal/diag"
)

type Node struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	RoomID string `json:"room_id,omitempty"`
	Label  string `json:"label,omitempty"`
}

type DependencyGraph struct {
	Nodes   map[string]Node            `json:"nodes"`
	Edges   map[string]map[string]bool `json:"edges"`
	Reverse map[string]map[string]bool `json:"reverse"`
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{
		Nodes:   make(map[string]Node),
		Edges:   make(map[string]map[string]bool),
		Reverse: make(map[string]map[string]bool),
	}
}

func (g *DependencyGraph) AddNode(node Node) {
	if _, exists := g.Nodes[node.ID]; !exists {
		g.Nodes[node.ID] = node
	}
	if g.Edges[node.ID] == nil {
		g.Edges[node.ID] = make(map[string]bool)
	}
	if g.Reverse[node.ID] == nil {
		g.Reverse[node.ID] = make(map[string]bool)
	}
}

func (g *DependencyGraph) AddEdge(dependentID, dependencyID string) {
	g.AddNode(Node{ID: dependentID})
	g.AddNode(Node{ID: dependencyID})
	g.Edges[dependentID][dependencyID] = true
	g.Reverse[dependencyID][dependentID] = true
}

func (g *DependencyGraph) AddDependency(dependentID, dependencyID string) {
	g.AddEdge(dependentID, dependencyID)
}

func (g *DependencyGraph) ForwardClosure(changed ...string) []string {
	affected := make(map[string]bool)
	queue := append([]string{}, changed...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for dependent := range g.Reverse[id] {
			if !affected[dependent] {
				affected[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	out := make([]string, 0, len(affected))
	for id := range affected {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (g *DependencyGraph) TopologicalOrder(ids []string) ([]string, error) {
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	indegree := make(map[string]int, len(ids))
	for id := range requested {
		indegree[id] = 0
	}
	for dependent := range requested {
		for dependency := range g.Edges[dependent] {
			if requested[dependency] {
				indegree[dependent]++
			}
		}
	}
	ready := make([]string, 0)
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(ids))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for dependent := range g.Reverse[id] {
			if !requested[dependent] {
				continue
			}
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(ids) {
		return nil, diag.New(diag.CodeState, "dependency graph contains a cycle")
	}
	return order, nil
}

func RadianceSourceNode(probeID string) string { return "source:radiance:" + probeID }
func SHSourceNode(cellID string) string        { return "source:sh:" + cellID }
func ReconNode(probeID string) string          { return "recon:" + probeID }
func InterpNode(queryID string) string         { return "interp:" + queryID }
func BlendNode(queryID string) string          { return "blend:" + queryID }
func PixelNode(pixelID string) string          { return "pixel:" + pixelID }
