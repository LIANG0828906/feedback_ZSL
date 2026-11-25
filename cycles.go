package orchestrator

import "sort"

// FindDependencyCycles reports every strongly connected component that forms a
// dependency cycle. Each returned cycle starts at its smallest node and repeats
// that node at the end, for example [a b c a].
func FindDependencyCycles(project *Project) [][]string {
	if project == nil {
		return nil
	}
	adjacency := make(map[string][]string)
	nodes := make(map[string]struct{})
	for _, node := range project.Nodes {
		nodes[node.ID] = struct{}{}
	}
	for _, edge := range project.Edges {
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
	}
	for node := range adjacency {
		sort.Strings(adjacency[node])
	}

	index := 0
	indices := make(map[string]int)
	lowlink := make(map[string]int)
	onStack := make(map[string]bool)
	var stack []string
	var components [][]string
	var visit func(string)
	visit = func(node string) {
		indices[node] = index
		lowlink[node] = index
		index++
		stack = append(stack, node)
		onStack[node] = true
		for _, next := range adjacency[node] {
			if _, seen := indices[next]; !seen {
				visit(next)
				if lowlink[next] < lowlink[node] {
					lowlink[node] = lowlink[next]
				}
			} else if onStack[next] && indices[next] < lowlink[node] {
				lowlink[node] = indices[next]
			}
		}
		if lowlink[node] == indices[node] {
			var component []string
			for {
				last := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[last] = false
				component = append(component, last)
				if last == node {
					break
				}
			}
			sort.Strings(component)
			components = append(components, component)
		}
	}
	nodeList := make([]string, 0, len(nodes))
	for node := range nodes {
		nodeList = append(nodeList, node)
	}
	sort.Strings(nodeList)
	for _, node := range nodeList {
		if _, seen := indices[node]; !seen {
			visit(node)
		}
	}

	var cycles [][]string
	for _, component := range components {
		selfLoop := false
		if len(component) == 1 {
			for _, next := range adjacency[component[0]] {
				if next == component[0] {
					selfLoop = true
					break
				}
			}
		}
		if len(component) < 2 && !selfLoop {
			continue
		}
		if cycle := canonicalCycle(component, adjacency); len(cycle) > 0 {
			cycles = append(cycles, cycle)
		}
	}
	sort.Slice(cycles, func(i, j int) bool { return cycles[i][0] < cycles[j][0] })
	return cycles
}

func canonicalCycle(component []string, adjacency map[string][]string) []string {
	allowed := make(map[string]bool, len(component))
	for _, node := range component {
		allowed[node] = true
	}
	start := component[0]
	path := []string{start}
	visited := map[string]bool{start: true}
	var search func(string) []string
	search = func(current string) []string {
		for _, next := range adjacency[current] {
			if !allowed[next] {
				continue
			}
			if next == start {
				result := append([]string(nil), path...)
				return append(result, start)
			}
			if visited[next] {
				continue
			}
			visited[next] = true
			path = append(path, next)
			if result := search(next); len(result) > 0 {
				return result
			}
			path = path[:len(path)-1]
			delete(visited, next)
		}
		return nil
	}
	return search(start)
}
