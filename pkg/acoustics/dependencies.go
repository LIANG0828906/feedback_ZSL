package acoustics

import "sort"

func BuildDependencyGraph(level *Level, report *Report) *DependencyGraph {
	g := &DependencyGraph{}
	nodeSeen := map[string]bool{}
	addNode := func(n DependencyNode) {
		if nodeSeen[n.ID] {
			return
		}
		nodeSeen[n.ID] = true
		g.Nodes = append(g.Nodes, n)
	}
	addEdge := func(from, to, typ, reason string) {
		g.Edges = append(g.Edges, DependencyEdge{From: from, To: to, Type: typ, Reason: reason})
	}

	listenersByRoom := map[string][]Listener{}
	for _, l := range level.Listeners {
		listenersByRoom[l.RoomID] = append(listenersByRoom[l.RoomID], l)
		addNode(DependencyNode{ID: "listener:" + l.ID, Kind: "listener", RoomID: l.RoomID, Label: l.ID})
	}
	for _, r := range level.Rooms {
		addNode(DependencyNode{ID: "room:" + r.ID, Kind: "room", RoomID: r.ID, Label: r.ID})
	}
	for _, ref := range level.Reflectors {
		id := "surface:" + ref.ID
		addNode(DependencyNode{ID: id, Kind: "surface", RoomID: ref.RoomID, Label: ref.ID})
		addEdge("room:"+ref.RoomID, id, "owns_surface", "frozen reflector snapshot")
		for _, lis := range listenersByRoom[ref.RoomID] {
			addEdge(id, "listener:"+lis.ID, "reflection_input", "mirror reflection and absorption")
		}
	}
	for _, d := range level.Doors {
		id := "door:" + d.ID
		addNode(DependencyNode{ID: id, Kind: "door", Label: d.ID})
	}

	pairSeen := map[string]bool{}
	addPair := func(srcID, lisID string, chain *Chain) {
		pairNode := "chain:" + srcID + "->" + lisID
		if pairSeen[pairNode] {
			return
		}
		pairSeen[pairNode] = true
		addNode(DependencyNode{ID: pairNode, Kind: "chain", Label: srcID + "->" + lisID})
		addEdge(pairNode, "listener:"+lisID, "chain_to_listener", "door prefix and effective input point")
		if chain != nil {
			for _, doorID := range chain.DoorIDs {
				addEdge("door:"+doorID, pairNode, "door_chain", "actual opening used by shortest chain")
			}
		}
	}

	if report != nil {
		for _, pr := range report.Pairs {
			addPair(pr.SourceID, pr.ListenerID, pr.Chain)
		}
	} else {
		for _, s := range level.Sources {
			for _, l := range level.Listeners {
				addPair(s.ID, l.ID, nil)
			}
		}
	}

	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		if g.Edges[i].To != g.Edges[j].To {
			return g.Edges[i].To < g.Edges[j].To
		}
		return g.Edges[i].Type < g.Edges[j].Type
	})
	return g
}
