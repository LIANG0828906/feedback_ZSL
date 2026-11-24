package core

import "sort"

// TopoOrder returns parents before children, breaking ties by node ID.
func TopoOrder(s *Scenario) []string {
	nodes := s.NodeMap()
	children := make(map[string][]string, len(nodes))
	roots := make([]string, 0, len(nodes))
	for _, n := range s.Nodes {
		if n.ParentID == "" {
			roots = append(roots, n.ID)
			continue
		}
		children[n.ParentID] = append(children[n.ParentID], n.ID)
	}
	for id := range children {
		sort.Strings(children[id])
	}
	sort.Strings(roots)
	queue := append([]string(nil), roots...)
	result := make([]string, 0, len(nodes))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		result = append(result, id)
		next := children[id]
		queue = append(queue, next...)
		sort.Strings(queue)
	}
	return result
}
