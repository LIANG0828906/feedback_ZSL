package dag

import (
	"sort"
)

type Node struct {
	Key          string   `json:"key"`
	Kind         string   `json:"kind"`
	DialogueID   string   `json:"dialogue_id"`
	PhonemeIndex int      `json:"phoneme_index"`
	DependsOn    []string `json:"depends_on,omitempty"`
}

type Graph struct {
	nodes      map[string]Node
	dependents map[string]map[string]struct{}
}

func New() *Graph {
	return &Graph{nodes: map[string]Node{}, dependents: map[string]map[string]struct{}{}}
}

func (g *Graph) Add(node Node) Node {
	deps := append([]string(nil), node.DependsOn...)
	sort.Strings(deps)
	node.DependsOn = deps
	g.nodes[node.Key] = node
	for _, dep := range deps {
		if g.dependents[dep] == nil {
			g.dependents[dep] = map[string]struct{}{}
		}
		g.dependents[dep][node.Key] = struct{}{}
	}
	return node
}

func (g *Graph) Get(key string) (Node, bool) {
	n, ok := g.nodes[key]
	return n, ok
}

func (g *Graph) Nodes() []Node {
	out := make([]Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Invalidate returns the changed keys and every transitive dependent key.
// Traversal is key-based and therefore independent of goroutine completion.
func (g *Graph) Invalidate(changed ...string) []string {
	invalid := map[string]struct{}{}
	queue := append([]string(nil), changed...)
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		if _, seen := invalid[key]; seen {
			continue
		}
		invalid[key] = struct{}{}
		for dependent := range g.dependents[key] {
			if _, seen := invalid[dependent]; !seen {
				queue = append(queue, dependent)
			}
		}
	}
	out := make([]string, 0, len(invalid))
	for key := range invalid {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// TopologicalOrder returns invalid nodes in dependency order. Unknown changed
// roots are preserved as roots but are not treated as graph nodes.
func (g *Graph) TopologicalOrder(invalid []string) []Node {
	set := map[string]struct{}{}
	for _, key := range invalid {
		set[key] = struct{}{}
	}
	indegree := map[string]int{}
	for key := range set {
		if node, ok := g.nodes[key]; ok {
			for _, dep := range node.DependsOn {
				if _, affected := set[dep]; affected {
					indegree[key]++
				}
			}
		}
		if _, ok := indegree[key]; !ok {
			indegree[key] = 0
		}
	}
	ready := make([]string, 0)
	for key, degree := range indegree {
		if degree == 0 {
			ready = append(ready, key)
		}
	}
	sort.Strings(ready)
	out := make([]Node, 0, len(set))
	for len(ready) > 0 {
		key := ready[0]
		ready = ready[1:]
		if node, ok := g.nodes[key]; ok {
			out = append(out, node)
		}
		for dependent := range g.dependents[key] {
			if _, affected := set[dependent]; !affected {
				continue
			}
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Strings(ready)
			}
		}
	}
	return out
}
