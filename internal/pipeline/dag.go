package pipeline

import (
	"fmt"
	"sort"
)

type DAG struct {
	nodes map[string]map[string]bool
}

func NewDAG() *DAG { return &DAG{nodes: make(map[string]map[string]bool)} }

func (d *DAG) addNode(node string) {
	if d.nodes[node] == nil {
		d.nodes[node] = make(map[string]bool)
	}
}

func (d *DAG) AddEdge(from, to string) {
	d.addNode(from)
	d.addNode(to)
	d.nodes[from][to] = true
}

func (d *DAG) TopologicalOrder() []string {
	indegree := make(map[string]int, len(d.nodes))
	for node := range d.nodes {
		indegree[node] = 0
	}
	for _, nexts := range d.nodes {
		for next := range nexts {
			indegree[next]++
		}
	}
	var ready []string
	for node, deg := range indegree {
		if deg == 0 {
			ready = append(ready, node)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		node := ready[0]
		ready = ready[1:]
		order = append(order, node)
		var newly []string
		for next := range d.nodes[node] {
			indegree[next]--
			if indegree[next] == 0 {
				newly = append(newly, next)
			}
		}
		sort.Strings(newly)
		ready = append(ready, newly...)
		sort.Strings(ready)
	}
	return order
}

func BuildTopology(tileIDs []int, patchIDs []string) []string {
	dag := NewDAG()
	for _, id := range patchIDs {
		dag.AddEdge("version:"+id, "validate:"+id)
		dag.AddEdge("validate:"+id, "mesh:"+id)
		dag.AddEdge("mesh:"+id, "project:"+id)
	}
	for _, tile := range tileIDs {
		tileNode := tileNodeName(tile)
		for _, id := range patchIDs {
			dag.AddEdge("project:"+id, tileNode)
		}
		dag.AddEdge(tileNode, "commit:"+tileNodeName(tile))
	}
	return dag.TopologicalOrder()
}

func tileNodeName(id int) string {
	return fmt.Sprintf("tile:%04d", id)
}
