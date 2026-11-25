package acoustic

import (
	"fmt"
	"sort"
)

type DependencyKind string

const (
	DependencyRoom          DependencyKind = "room"
	DependencyReflector     DependencyKind = "reflector"
	DependencyDoor          DependencyKind = "door"
	DependencySource        DependencyKind = "source"
	DependencyListener      DependencyKind = "listener"
	DependencyChain         DependencyKind = "chain"
	DependencyListenerSolve DependencyKind = "listener_solve"
)

type DependencyNode struct {
	Kind        DependencyKind `json:"kind"`
	RoomID      int            `json:"roomId,omitempty"`
	ReflectorID int            `json:"reflectorId,omitempty"`
	OpeningID   int            `json:"openingId,omitempty"`
	SourceID    int            `json:"sourceId,omitempty"`
	ListenerID  int            `json:"listenerId,omitempty"`
}

func (n DependencyNode) String() string {
	return fmt.Sprintf("%s:room=%d:reflector=%d:door=%d:source=%d:listener=%d",
		n.Kind, n.RoomID, n.ReflectorID, n.OpeningID, n.SourceID, n.ListenerID)
}

type DependencyEdge struct {
	From DependencyNode `json:"from"`
	To   DependencyNode `json:"to"`
}

type DependencyGraph struct {
	edges map[DependencyNode]map[DependencyNode]struct{}
}

func NewDependencyGraph() *DependencyGraph {
	return &DependencyGraph{edges: make(map[DependencyNode]map[DependencyNode]struct{})}
}

func (g *DependencyGraph) Add(from, to DependencyNode) {
	if g.edges[from] == nil {
		g.edges[from] = make(map[DependencyNode]struct{})
	}
	g.edges[from][to] = struct{}{}
}

func (g *DependencyGraph) Edges() []DependencyEdge {
	var edges []DependencyEdge
	for from, targets := range g.edges {
		for to := range targets {
			edges = append(edges, DependencyEdge{From: from, To: to})
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From.String() != edges[j].From.String() {
			return edges[i].From.String() < edges[j].From.String()
		}
		return edges[i].To.String() < edges[j].To.String()
	})
	return edges
}

func (g *DependencyGraph) Downstream(roots []DependencyNode) map[DependencyNode]bool {
	invalidated := make(map[DependencyNode]bool)
	queue := append([]DependencyNode(nil), roots...)
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if invalidated[node] {
			continue
		}
		invalidated[node] = true
		for target := range g.edges[node] {
			if !invalidated[target] {
				queue = append(queue, target)
			}
		}
	}
	return invalidated
}

type InvalidationEntry struct {
	RoomID               int      `json:"roomId"`
	Invalidated          bool     `json:"invalidated"`
	Reused               bool     `json:"reused"`
	Reasons              []string `json:"reasons,omitempty"`
	InvalidatedListeners []int    `json:"invalidatedListeners,omitempty"`
}

type InvalidationReport struct {
	Entries             []InvalidationEntry `json:"entries"`
	InvalidatedRooms    []int               `json:"invalidatedRooms"`
	ReusedRooms         []int               `json:"reusedRooms"`
	Dependencies        []DependencyEdge    `json:"dependencies"`
	ConnectivityChanged bool                `json:"connectivityChanged"`
}

func PlanInvalidation(oldProject, newProject *Project, cachedSolutions []ListenerSolution) (InvalidationReport, error) {
	if oldProject == nil || newProject == nil {
		return InvalidationReport{}, &Error{Code: CodeSchema, Op: "invalidation", Message: "both projects are required"}
	}
	if err := oldProject.Validate(); err != nil {
		return InvalidationReport{}, err
	}
	if err := newProject.Validate(); err != nil {
		return InvalidationReport{}, err
	}
	cached := make(map[int]ListenerSolution)
	for _, solution := range cachedSolutions {
		cached[solution.ListenerID] = solution
	}
	graph := NewDependencyGraph()
	rootSet := make(map[DependencyNode]struct{})
	roomReasons := make(map[int][]string)
	invalidatedListeners := make(map[int]map[int]struct{})
	newChains := make(map[[2]int]ChainSummary)
	connectivityChanged := false

	addRoot := func(node DependencyNode) {
		rootSet[node] = struct{}{}
	}
	addReason := func(roomID int, reason string) {
		roomReasons[roomID] = append(roomReasons[roomID], reason)
	}
	addInvalidListener := func(roomID, listenerID int) {
		if invalidatedListeners[roomID] == nil {
			invalidatedListeners[roomID] = make(map[int]struct{})
		}
		invalidatedListeners[roomID][listenerID] = struct{}{}
	}

	oldRooms := roomMap(oldProject.Rooms)
	newRooms := roomMap(newProject.Rooms)
	for roomID, newRoom := range newRooms {
		roomNode := DependencyNode{Kind: DependencyRoom, RoomID: roomID}
		oldRoom, ok := oldRooms[roomID]
		if !ok || oldRoom.Rect != newRoom.Rect {
			addRoot(roomNode)
			addReason(roomID, "room boundary or room identity changed")
		}
		for _, reflector := range newProject.Reflectors {
			if reflector.RoomID != roomID {
				continue
			}
			reflectorNode := DependencyNode{Kind: DependencyReflector, RoomID: roomID, ReflectorID: reflector.ID}
			graph.Add(roomNode, reflectorNode)
			for _, listener := range newProject.Listeners {
				if listener.RoomID != roomID {
					continue
				}
				for _, source := range newProject.Sources {
					solveNode := DependencyNode{Kind: DependencyListenerSolve, RoomID: roomID, ListenerID: listener.ID, SourceID: source.ID}
					graph.Add(reflectorNode, solveNode)
				}
			}
			if oldReflector, ok := oldProject.reflectorByID(reflector.ID); !ok || !reflectorsEqual(*oldReflector, reflector) {
				addRoot(reflectorNode)
				addReason(roomID, fmt.Sprintf("reflector %d parameters changed", reflector.ID))
			}
		}
		for _, oldReflector := range oldProject.Reflectors {
			if oldReflector.RoomID != roomID {
				continue
			}
			newReflector, ok := newProject.reflectorByID(oldReflector.ID)
			if !ok || newReflector.RoomID != roomID {
				for _, listener := range newProject.Listeners {
					if listener.RoomID != roomID {
						continue
					}
					for _, source := range newProject.Sources {
						addRoot(DependencyNode{Kind: DependencyListenerSolve, RoomID: roomID, ListenerID: listener.ID, SourceID: source.ID})
					}
				}
				addReason(roomID, fmt.Sprintf("reflector %d was removed or moved", oldReflector.ID))
			}
		}
		for _, listener := range newProject.Listeners {
			if listener.RoomID != roomID {
				continue
			}
			listenerNode := DependencyNode{Kind: DependencyListener, RoomID: roomID, ListenerID: listener.ID}
			graph.Add(roomNode, listenerNode)
			if oldListener, ok := oldProject.listenerByID(listener.ID); !ok || !listenersEqual(*oldListener, listener) {
				addRoot(listenerNode)
				addReason(roomID, fmt.Sprintf("listener %d position changed", listener.ID))
			}
			for _, source := range newProject.Sources {
				solveNode := DependencyNode{Kind: DependencyListenerSolve, RoomID: roomID, ListenerID: listener.ID, SourceID: source.ID}
				graph.Add(listenerNode, solveNode)
			}
		}
		for _, oldListener := range oldProject.Listeners {
			if oldListener.RoomID != roomID {
				continue
			}
			newListener, ok := newProject.listenerByID(oldListener.ID)
			if !ok || newListener.RoomID != roomID {
				addRoot(roomNode)
				addReason(roomID, fmt.Sprintf("listener %d was removed or moved", oldListener.ID))
			}
		}
	}

	oldSources := sourceMap(oldProject.Sources)
	newSources := sourceMap(newProject.Sources)
	for sourceID, source := range newSources {
		sourceNode := DependencyNode{Kind: DependencySource, SourceID: sourceID}
		oldSource, ok := oldSources[sourceID]
		if !ok || !sourcesEqual(oldSource, source) {
			addRoot(sourceNode)
			for _, listener := range newProject.Listeners {
				addReason(listener.RoomID, fmt.Sprintf("source %d parameters changed", sourceID))
			}
		}
		for _, listener := range newProject.Listeners {
			chainNode := DependencyNode{Kind: DependencyChain, SourceID: sourceID, ListenerID: listener.ID}
			graph.Add(sourceNode, chainNode)
		}
	}
	for sourceID := range oldSources {
		if _, ok := newSources[sourceID]; ok {
			continue
		}
		for _, listener := range newProject.Listeners {
			addReason(listener.RoomID, fmt.Sprintf("source %d was removed", sourceID))
			addRoot(DependencyNode{Kind: DependencyRoom, RoomID: listener.RoomID})
		}
	}

	for _, listener := range newProject.Listeners {
		cachedSolution, ok := cached[listener.ID]
		if !ok {
			addReason(listener.RoomID, fmt.Sprintf("listener %d has no cached solution", listener.ID))
			for _, source := range newProject.Sources {
				solveNode := DependencyNode{Kind: DependencyListenerSolve, RoomID: listener.RoomID, ListenerID: listener.ID, SourceID: source.ID}
				addRoot(solveNode)
			}
			continue
		}
		oldChains := make(map[int]ChainSummary, len(cachedSolution.Sources))
		for _, sourceResult := range cachedSolution.Sources {
			oldChains[sourceResult.SourceID] = sourceResult.Chain
		}
		for _, source := range newProject.Sources {
			chainNode := DependencyNode{Kind: DependencyChain, SourceID: source.ID, ListenerID: listener.ID}
			solveNode := DependencyNode{Kind: DependencyListenerSolve, RoomID: listener.RoomID, ListenerID: listener.ID, SourceID: source.ID}
			graph.Add(chainNode, solveNode)
			newChain, err := FindChain(newProject, source.ID, listener.ID)
			if err == nil {
				newChains[[2]int{source.ID, listener.ID}] = newChain
			}
			oldChain, hadOld := oldChains[source.ID]
			if !hadOld || err != nil || !chainsEqual(oldChain, newChain) {
				addRoot(chainNode)
				addReason(listener.RoomID, fmt.Sprintf("room chain changed for listener %d from source %d", listener.ID, source.ID))
				connectivityChanged = true
				addInvalidListener(listener.RoomID, listener.ID)
			}
		}
	}

	chainsByKey := make(map[[2]int]ChainSummary)
	for _, chain := range oldChainSummaries(cachedSolutions) {
		chainsByKey[[2]int{chain.SourceID, chain.ListenerID}] = chain
	}
	for key, chain := range newChains {
		chainsByKey[key] = chain
	}
	for _, chain := range chainsByKey {
		chainNode := DependencyNode{Kind: DependencyChain, SourceID: chain.SourceID, ListenerID: chain.ListenerID}
		for _, roomID := range chain.RoomIDs {
			graph.Add(DependencyNode{Kind: DependencyRoom, RoomID: roomID}, chainNode)
		}
		for _, openingID := range chain.OpeningIDs {
			graph.Add(DependencyNode{Kind: DependencyDoor, OpeningID: openingID}, chainNode)
		}
	}

	invalidated := graph.Downstream(sortedRoots(rootSet))
	report := InvalidationReport{
		ConnectivityChanged: connectivityChanged,
		Dependencies:        graph.Edges(),
	}
	roomIDs := sortedRoomIDs(newRooms)
	for _, roomID := range roomIDs {
		reasons := append([]string(nil), roomReasons[roomID]...)
		roomInvalid := false
		if invalidated[DependencyNode{Kind: DependencyRoom, RoomID: roomID}] {
			roomInvalid = true
		}
		for _, reflector := range newProject.Reflectors {
			if reflector.RoomID == roomID && invalidated[DependencyNode{Kind: DependencyReflector, RoomID: roomID, ReflectorID: reflector.ID}] {
				roomInvalid = true
			}
		}
		for _, listener := range newProject.Listeners {
			if listener.RoomID != roomID {
				continue
			}
			for _, source := range newProject.Sources {
				if invalidated[DependencyNode{Kind: DependencyListenerSolve, RoomID: roomID, ListenerID: listener.ID, SourceID: source.ID}] {
					roomInvalid = true
					addInvalidListener(roomID, listener.ID)
				}
			}
		}
		listenersForEntry := make([]int, 0)
		for listenerID := range invalidatedListeners[roomID] {
			listenersForEntry = append(listenersForEntry, listenerID)
		}
		sort.Ints(listenersForEntry)
		if roomInvalid && len(reasons) == 0 {
			reasons = append(reasons, "downstream dependency invalidation")
		}
		entry := InvalidationEntry{
			RoomID:               roomID,
			Invalidated:          roomInvalid,
			Reused:               !roomInvalid,
			Reasons:              uniqueStrings(reasons),
			InvalidatedListeners: listenersForEntry,
		}
		report.Entries = append(report.Entries, entry)
		if roomInvalid {
			report.InvalidatedRooms = append(report.InvalidatedRooms, roomID)
		} else {
			report.ReusedRooms = append(report.ReusedRooms, roomID)
		}
	}
	return report, nil
}

func reflectorsEqual(a, b Reflector) bool {
	return a.ID == b.ID && a.RoomID == b.RoomID && a.A == b.A && a.B == b.B && a.NX == b.NX && a.NY == b.NY && a.Absorption == b.Absorption
}

func sourcesEqual(a, b Source) bool {
	return a.ID == b.ID && a.RoomID == b.RoomID && a.Pos == b.Pos && a.LevelDB == b.LevelDB
}

func listenersEqual(a, b Listener) bool {
	return a.ID == b.ID && a.RoomID == b.RoomID && a.Pos == b.Pos
}

func chainsEqual(a, b ChainSummary) bool {
	if a.LengthTicks != b.LengthTicks || len(a.RoomIDs) != len(b.RoomIDs) || len(a.OpeningIDs) != len(b.OpeningIDs) {
		return false
	}
	for i := range a.RoomIDs {
		if a.RoomIDs[i] != b.RoomIDs[i] {
			return false
		}
	}
	for i := range a.OpeningIDs {
		if a.OpeningIDs[i] != b.OpeningIDs[i] {
			return false
		}
	}
	return true
}

func roomMap(rooms []Room) map[int]Room {
	result := make(map[int]Room, len(rooms))
	for _, room := range rooms {
		result[room.ID] = room
	}
	return result
}

func sourceMap(sources []Source) map[int]Source {
	result := make(map[int]Source, len(sources))
	for _, source := range sources {
		result[source.ID] = source
	}
	return result
}

func openingMap(openings []Opening) map[int]Opening {
	result := make(map[int]Opening, len(openings))
	for _, opening := range openings {
		result[opening.ID] = opening
	}
	return result
}

func sortedRoomIDs(rooms map[int]Room) []int {
	ids := make([]int, 0, len(rooms))
	for roomID := range rooms {
		ids = append(ids, roomID)
	}
	sort.Ints(ids)
	return ids
}

func sortedRoots(rootSet map[DependencyNode]struct{}) []DependencyNode {
	roots := make([]DependencyNode, 0, len(rootSet))
	for node := range rootSet {
		roots = append(roots, node)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].String() < roots[j].String() })
	return roots
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func oldChainSummaries(solutions []ListenerSolution) []ChainSummary {
	var chains []ChainSummary
	for _, solution := range solutions {
		for _, source := range solution.Sources {
			chains = append(chains, source.Chain)
		}
	}
	return chains
}
