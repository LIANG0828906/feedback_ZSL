package acoustics

import (
	"sort"
	"strings"

	"roomacoustics/pkg/fixedpoint"
)

// Chain is the deterministic door-connected route for one source/listener pair.
type Chain struct {
	SourceID       string   `json:"source_id,omitempty"`
	ListenerID     string   `json:"listener_id,omitempty"`
	SourceRoomID   string   `json:"source_room_id"`
	ListenerRoomID string   `json:"listener_room_id"`
	Connected      bool     `json:"connected"`
	RoomIDs        []string `json:"room_ids"`
	DoorIDs        []string `json:"door_ids"`
	LengthFixed    int64    `json:"length_fixed"`
	LengthM        float64  `json:"length_m"`
}

type SearchedChain struct {
	RoomIDs []string `json:"room_ids"`
	DoorIDs []string `json:"door_ids,omitempty"`
}

type UnreachableDetails struct {
	PairID         string          `json:"pair_id"`
	SourceRoomID   string          `json:"source_room_id"`
	ListenerRoomID string          `json:"listener_room_id"`
	VisitedRooms   []string        `json:"visited_rooms"`
	SearchedChains []SearchedChain `json:"searched_chains"`
}

type fixedRoute struct {
	roomIDs     []string
	doors       []Door
	doorIDs     []string
	lengthFixed int64
}

// SolveSourceListenerChain resolves IDs and then runs the fixed-point solver.
func (idx *Index) SolveSourceListenerChain(sourceID, listenerID string) (*Chain, error) {
	s, ok := idx.SourcesByID[sourceID]
	if !ok {
		return nil, Errorf(CodeSchema, sourceID, "source_id", "unknown source")
	}
	l, ok := idx.ListenersByID[listenerID]
	if !ok {
		return nil, Errorf(CodeSchema, listenerID, "listener_id", "unknown listener")
	}
	return idx.SolvePointChain(sourceID, listenerID, s.Position, s.RoomID, l.Position, l.RoomID)
}

// SolvePointChain finds the minimum-hop room chain and shortest fixed-point
// polyline through its actual door openings.
func (idx *Index) SolvePointChain(sourceID, listenerID string, source Point, sourceRoomID string, listener Point, listenerRoomID string) (*Chain, error) {
	src := fixedpoint.FromMeters(source.X, source.Y)
	dst := fixedpoint.FromMeters(listener.X, listener.Y)

	if sourceRoomID == listenerRoomID {
		room, ok := idx.RoomByID[sourceRoomID]
		if !ok {
			return nil, Errorf(CodeSchema, sourceRoomID, "room_id", "unknown source room")
		}
		if !fixedPointInRect(src, room.Rect) {
			return nil, Errorf(CodeGeom, sourceRoomID, "position", "source is outside room in fixed kernel")
		}
		length := fixedpoint.Distance(src, dst)
		return &Chain{
			SourceID: sourceID, ListenerID: listenerID,
			SourceRoomID: sourceRoomID, ListenerRoomID: listenerRoomID,
			Connected: true, RoomIDs: []string{sourceRoomID}, DoorIDs: []string{},
			LengthFixed: length, LengthM: fixedToMeters(length),
		}, nil
	}

	dist, parents := idx.bfsShortest(sourceRoomID)
	if _, reachable := dist[listenerRoomID]; !reachable {
		return nil, idx.unreachableError(sourceID, listenerID, sourceRoomID, listenerRoomID, parents)
	}

	var best *fixedRoute
	var visitPaths func(path []string)
	visitPaths = func(path []string) {
		last := path[len(path)-1]
		if last == listenerRoomID {
			route, err := idx.bestDoorsForRoomPath(path, src, dst)
			if err == nil && (best == nil || route.lengthFixed < best.lengthFixed ||
				(route.lengthFixed == best.lengthFixed && strings.Join(route.roomIDs, "|") < strings.Join(best.roomIDs, "|")) ||
				(route.lengthFixed == best.lengthFixed && strings.Join(route.roomIDs, "|") == strings.Join(best.roomIDs, "|") &&
					strings.Join(route.doorIDs, "|") < strings.Join(best.doorIDs, "|"))) {
				best = route
			}
			return
		}
		for _, next := range idx.adjacentRooms(last) {
			if d, ok := dist[next.room]; ok && d == dist[last]+1 && !containsString(path, next.room) {
				visitPaths(append(append([]string{}, path...), next.room))
			}
		}
	}
	visitPaths([]string{sourceRoomID})
	if best == nil {
		return nil, idx.unreachableError(sourceID, listenerID, sourceRoomID, listenerRoomID, parents)
	}
	if err := idx.validateRouteGeometry(best, src, dst); err != nil {
		return nil, err
	}

	return &Chain{
		SourceID: sourceID, ListenerID: listenerID,
		SourceRoomID: sourceRoomID, ListenerRoomID: listenerRoomID,
		Connected: true, RoomIDs: append([]string{}, best.roomIDs...), DoorIDs: append([]string{}, best.doorIDs...),
		LengthFixed: best.lengthFixed, LengthM: fixedToMeters(best.lengthFixed),
	}, nil
}

type bfsParent struct {
	room string
	door string
}

func (idx *Index) bfsShortest(start string) (map[string]int, map[string]bfsParent) {
	dist := map[string]int{start: 0}
	parent := map[string]bfsParent{start: {}}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		neighbors := idx.adjacentRooms(cur)
		for _, next := range neighbors {
			if _, seen := dist[next.room]; !seen {
				dist[next.room] = dist[cur] + 1
				parent[next.room] = bfsParent{room: cur, door: next.doorID}
				queue = append(queue, next.room)
			}
		}
	}
	return dist, parent
}

type adjacentRoom struct {
	room   string
	doorID string
}

func (idx *Index) adjacentRooms(roomID string) []adjacentRoom {
	var out []adjacentRoom
	seen := map[string]bool{}
	for _, d := range idx.DoorsByRoom[roomID] {
		w, ok := idx.WallByID[d.WallID]
		if !ok {
			continue
		}
		other := w.RoomA
		if other == roomID {
			other = w.RoomB
		} else if w.RoomB != roomID {
			continue
		}
		key := other + "\x00" + d.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, adjacentRoom{room: other, doorID: d.ID})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].room != out[j].room {
			return out[i].room < out[j].room
		}
		return out[i].doorID < out[j].doorID
	})
	return out
}

func (idx *Index) unreachableError(sourceID, listenerID, srcRoom, dstRoom string, parents map[string]bfsParent) error {
	visited := make([]string, 0, len(parents))
	for room := range parents {
		visited = append(visited, room)
	}
	sort.Strings(visited)
	chains := make([]SearchedChain, 0, len(visited))
	for _, room := range visited {
		rooms, doors := reconstructBFSChain(room, parents)
		chains = append(chains, SearchedChain{RoomIDs: rooms, DoorIDs: doors})
	}
	return &DomainError{
		Code:     CodeUnreach,
		ObjectID: "pair:" + sourceID + "->" + listenerID,
		Field:    "room_chain",
		Message:  "no open door chain from " + srcRoom + " to " + dstRoom,
		Details: UnreachableDetails{
			PairID: "pair:" + sourceID + "->" + listenerID, SourceRoomID: srcRoom, ListenerRoomID: dstRoom,
			VisitedRooms: visited, SearchedChains: chains,
		},
	}
}

func reconstructBFSChain(target string, parents map[string]bfsParent) ([]string, []string) {
	var rooms []string
	var doors []string
	cur := target
	for {
		p, ok := parents[cur]
		if !ok {
			break
		}
		rooms = append([]string{cur}, rooms...)
		if p.room == "" {
			break
		}
		doors = append([]string{p.door}, doors...)
		cur = p.room
	}
	return rooms, doors
}

type doorDPState struct {
	door    Door
	cost    int64
	doorIDs []string
}

func (idx *Index) bestDoorsForRoomPath(roomPath []string, src, dst fixedpoint.V) (*fixedRoute, error) {
	if len(roomPath) < 2 {
		length := fixedpoint.Distance(src, dst)
		return &fixedRoute{roomIDs: append([]string{}, roomPath...), doorIDs: []string{}, lengthFixed: length}, nil
	}
	transitions := make([][]Door, 0, len(roomPath)-1)
	for i := 0; i < len(roomPath)-1; i++ {
		doors := idx.DoorsBetween(roomPath[i], roomPath[i+1])
		if len(doors) == 0 {
			return nil, Errorf(CodeUnreach, strings.Join(roomPath, "->"), "door", "room chain adjacency has no actual door")
		}
		transitions = append(transitions, doors)
	}

	var states []doorDPState
	for _, d := range transitions[0] {
		c := fixedpoint.FromMeters(d.Center.X, d.Center.Y)
		states = append(states, doorDPState{door: d, cost: fixedpoint.Distance(src, c), doorIDs: []string{d.ID}})
	}
	for ti := 1; ti < len(transitions); ti++ {
		var nextStates []doorDPState
		for _, nd := range transitions[ti] {
			nc := fixedpoint.FromMeters(nd.Center.X, nd.Center.Y)
			var best *doorDPState
			var bestCost int64
			for si := range states {
				st := &states[si]
				sc := fixedpoint.FromMeters(st.door.Center.X, st.door.Center.Y)
				cost := st.cost + fixedpoint.Distance(sc, nc)
				ids := append(append([]string{}, st.doorIDs...), nd.ID)
				if best == nil || cost < bestCost || (cost == bestCost && strings.Join(ids, "|") < strings.Join(best.doorIDs, "|")) {
					newState := doorDPState{door: nd, cost: cost, doorIDs: ids}
					best = &newState
					bestCost = cost
				}
			}
			if best != nil {
				nextStates = append(nextStates, *best)
			}
		}
		states = nextStates
	}

	var bestFinal *doorDPState
	var bestLength int64
	for i := range states {
		st := &states[i]
		c := fixedpoint.FromMeters(st.door.Center.X, st.door.Center.Y)
		total := st.cost + fixedpoint.Distance(c, dst)
		if bestFinal == nil || total < bestLength || (total == bestLength && strings.Join(st.doorIDs, "|") < strings.Join(bestFinal.doorIDs, "|")) {
			cp := *st
			bestFinal = &cp
			bestLength = total
		}
	}
	if bestFinal == nil {
		return nil, Errorf(CodeUnreach, strings.Join(roomPath, "->"), "door", "no door combination found")
	}

	doorIDs := append([]string{}, bestFinal.doorIDs...)
	doors := make([]Door, 0, len(doorIDs))
	for _, id := range doorIDs {
		doors = append(doors, idx.DoorByID[id])
	}
	return &fixedRoute{roomIDs: append([]string{}, roomPath...), doors: doors, doorIDs: doorIDs, lengthFixed: bestLength}, nil
}

func (idx *Index) validateRouteGeometry(route *fixedRoute, src, dst fixedpoint.V) error {
	if len(route.roomIDs) == 1 {
		return nil
	}
	points := []fixedpoint.V{src}
	for _, d := range route.doors {
		points = append(points, fixedpoint.FromMeters(d.Center.X, d.Center.Y))
	}
	points = append(points, dst)
	for i := 0; i < len(points)-1; i++ {
		roomIndex := i
		if i >= len(route.roomIDs) {
			roomIndex = len(route.roomIDs) - 1
		}
		room := idx.RoomByID[route.roomIDs[roomIndex]]
		if !fixedSegmentInRect(points[i], points[i+1], room.Rect) {
			return Errorf(CodeGeom, route.roomIDs[roomIndex], "route", "computed segment crosses outside room/wall")
		}
	}
	return nil
}

func fixedPointInRect(p fixedpoint.V, r Rect) bool {
	min := fixedpoint.FromMeters(r.X1, r.Y1)
	max := fixedpoint.FromMeters(r.X2, r.Y2)
	return p.X >= min.X-1 && p.X <= max.X+1 && p.Y >= min.Y-1 && p.Y <= max.Y+1
}

func fixedSegmentInRect(a, b fixedpoint.V, r Rect) bool {
	// Rectangles are convex; containment of both endpoints proves containment.
	return fixedPointInRect(a, r) && fixedPointInRect(b, r)
}

func fixedToMeters(v int64) float64 { return float64(v) / float64(fixedpoint.Scale) }

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
