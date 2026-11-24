package wb

import "sort"

// TopologicalOrder returns deterministic Kahn order. dependencies maps a node
// to the nodes it depends on.
func TopologicalOrder(dependencies map[string][]string) ([]string, *APIError) {
	indegree := map[string]int{}
	children := map[string][]string{}
	for node, deps := range dependencies {
		if _, ok := indegree[node]; !ok {
			indegree[node] = 0
		}
		for _, dep := range deps {
			if _, ok := indegree[dep]; !ok {
				indegree[dep] = 0
			}
			indegree[node]++
			children[dep] = append(children[dep], node)
		}
	}

	ready := make([]string, 0)
	for node, degree := range indegree {
		if degree == 0 {
			ready = append(ready, node)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(indegree))
	for len(ready) > 0 {
		node := ready[0]
		ready = ready[1:]
		order = append(order, node)
		next := append([]string(nil), children[node]...)
		sort.Strings(next)
		for _, child := range next {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
			}
		}
		sort.Strings(ready)
	}
	if len(order) != len(indegree) {
		return nil, NewAPIError(CodeState, "dependency-graph", "", "dependency cycle detected")
	}
	return order, nil
}

func (e *Engine) buildDependenciesLocked() map[string][]string {
	deps := map[string][]string{}
	for _, room := range e.sample.Rooms {
		deps["param:"+room.ID] = nil
		deps["geom:"+room.ID] = nil
		deps["conn:"+room.ID] = []string{"geom:" + room.ID}
		deps["commit:"+room.ID] = []string{"conn:" + room.ID, "param:" + room.ID}
	}
	for _, o := range e.sample.Openings {
		deps["door:"+o.ID] = nil
		for _, roomID := range []string{o.RoomA, o.RoomB} {
			if roomID == "" {
				continue
			}
			connNode := "conn:" + roomID
			deps[connNode] = appendUnique(deps[connNode], "door:"+o.ID)
		}
	}
	for listenerID, result := range e.results {
		node := "listen:" + listenerID
		needed := []string{"param:" + result.RoomID, "conn:" + result.RoomID}
		for _, pair := range result.Pairs {
			if pair.Error != nil {
				continue
			}
			for _, roomID := range pair.Path.RoomChain {
				needed = appendUnique(needed, "conn:"+roomID)
			}
			for _, doorID := range pair.Path.DoorChain {
				needed = appendUnique(needed, "door:"+doorID)
			}
		}
		deps[node] = needed
		commitNode := "commit:" + result.RoomID
		deps[commitNode] = appendUnique(deps[commitNode], node)
	}
	return deps
}

func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}
