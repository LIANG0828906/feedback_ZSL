package topology

import (
	"math/big"

	"roomreverb/internal/fixedpoint"
	"roomreverb/internal/geom"
	"roomreverb/internal/model"
)

type NodeKind string

const (
	NodeSource   NodeKind = "source"
	NodeListener NodeKind = "listener"
	NodeDoor     NodeKind = "door"
)

type node struct {
	id   string
	kind NodeKind
	ref  string
	p    model.Point
}

type edge struct {
	to   int
	room string
	dist *big.Int
}

type Solution struct {
	SourceID   string
	ListenerID string
	Rooms      []string
	Doors      []string
	Points     []model.Point
	LengthQ    *big.Int
	Length     float64
	WallClear  bool
}

type Graph struct {
	Level    *model.Level
	Decimals int
	nodes    []node
	byID     map[string]int
	adj      [][]edge
}

func Build(level *model.Level, decimals int) *Graph {
	g := &Graph{Level: level, Decimals: decimals, byID: map[string]int{}}
	add := func(id string, kind NodeKind, ref string, p model.Point) int {
		if idx, ok := g.byID[id]; ok {
			return idx
		}
		idx := len(g.nodes)
		g.nodes = append(g.nodes, node{id: id, kind: kind, ref: ref, p: p})
		g.byID[id] = idx
		return idx
	}

	for _, source := range level.Sources {
		add("source:"+source.ID, NodeSource, source.ID, source.Pos)
	}
	for _, listener := range level.Listeners {
		add("listener:"+listener.ID, NodeListener, listener.ID, listener.Pos)
	}
	for _, door := range level.Doors {
		add("door:"+door.ID, NodeDoor, door.ID, geom.DoorMidpoint(door))
	}

	wallByID := map[string]model.Wall{}
	for _, wall := range level.Walls {
		wallByID[wall.ID] = wall
	}
	doorWall := map[string]string{}
	for _, door := range level.Doors {
		doorWall[door.ID] = door.WallID
	}

	type member struct {
		node int
		p    model.Point
	}
	for _, room := range level.Rooms {
		var members []member
		for _, source := range level.Sources {
			if geom.LocateRoom(level, source.Pos) == room.ID {
				members = append(members, member{g.byID["source:"+source.ID], source.Pos})
			}
		}
		for _, listener := range level.Listeners {
			if geom.LocateRoom(level, listener.Pos) == room.ID {
				members = append(members, member{g.byID["listener:"+listener.ID], listener.Pos})
			}
		}
		for _, door := range level.Doors {
			wall := wallByID[door.WallID]
			if wall.RoomAID == room.ID || wall.RoomBID == room.ID {
				idx := g.byID["door:"+door.ID]
				members = append(members, member{idx, geom.DoorMidpoint(door)})
			}
		}
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				dist := fixedpoint.SegmentLength(members[i].p, members[j].p, decimals)
				g.adj = ensure(g.adj, members[i].node+1)
				g.adj[members[i].node] = append(g.adj[members[i].node], edge{to: members[j].node, room: room.ID, dist: new(big.Int).Set(dist)})
				g.adj = ensure(g.adj, members[j].node+1)
				g.adj[members[j].node] = append(g.adj[members[j].node], edge{to: members[i].node, room: room.ID, dist: new(big.Int).Set(dist)})
			}
		}
	}
	return g
}

func ensure(adj [][]edge, n int) [][]edge {
	for len(adj) < n {
		adj = append(adj, nil)
	}
	return adj
}

func (g *Graph) Solve(sourceID, listenerID string) (*Solution, *model.Error) {
	startKey := "source:" + sourceID
	targetKey := "listener:" + listenerID
	start, ok := g.byID[startKey]
	if !ok {
		return nil, model.NewError(model.CodeUnreach, sourceID, "unknown source")
	}
	target, ok := g.byID[targetKey]
	if !ok {
		return nil, model.NewError(model.CodeUnreach, listenerID, "unknown listener")
	}

	n := len(g.nodes)
	dist := make([]*big.Int, n)
	visited := make([]bool, n)
	prev := make([]int, n)
	prevRoom := make([]string, n)
	for i := range prev {
		prev[i] = -1
	}
	zero := big.NewInt(0)
	dist[start] = zero

	for {
		cur := -1
		for i := 0; i < n; i++ {
			if visited[i] || dist[i] == nil {
				continue
			}
			if cur == -1 || dist[i].Cmp(dist[cur]) < 0 {
				cur = i
			}
		}
		if cur == -1 {
			break
		}
		if cur == target {
			break
		}
		visited[cur] = true
		for _, candidate := range g.adj[cur] {
			if visited[candidate.to] {
				continue
			}
			next := new(big.Int).Add(dist[cur], candidate.dist)
			if dist[candidate.to] == nil || next.Cmp(dist[candidate.to]) < 0 {
				dist[candidate.to] = next
				prev[candidate.to] = cur
				prevRoom[candidate.to] = candidate.room
			}
		}
	}

	if dist[target] == nil || prev[target] == -1 && target != start {
		return nil, model.NewError(model.CodeUnreach, sourceID+"/"+listenerID, "no door-connected room chain")
	}

	var nodePath []int
	for cur := target; cur != -1; cur = prev[cur] {
		nodePath = append(nodePath, cur)
		if cur == start {
			break
		}
	}
	for i, j := 0, len(nodePath)-1; i < j; i, j = i+1, j-1 {
		nodePath[i], nodePath[j] = nodePath[j], nodePath[i]
	}
	if len(nodePath) == 0 || nodePath[0] != start {
		return nil, model.NewError(model.CodeUnreach, sourceID+"/"+listenerID, "path reconstruction failed")
	}

	points := make([]model.Point, 0, len(nodePath))
	rooms := make([]string, 0)
	doors := make([]string, 0)
	edgeRoomsReverse := make([]string, 0)
	for cur := target; cur != start; cur = prev[cur] {
		edgeRoomsReverse = append(edgeRoomsReverse, prevRoom[cur])
	}
	for i, j := 0, len(edgeRoomsReverse)-1; i < j; i, j = i+1, j-1 {
		edgeRoomsReverse[i], edgeRoomsReverse[j] = edgeRoomsReverse[j], edgeRoomsReverse[i]
	}
	for i, idx := range nodePath {
		points = append(points, g.nodes[idx].p)
		if g.nodes[idx].kind == NodeDoor {
			doors = append(doors, g.nodes[idx].ref)
		}
		if i < len(edgeRoomsReverse) {
			room := edgeRoomsReverse[i]
			if len(rooms) == 0 || rooms[len(rooms)-1] != room {
				rooms = append(rooms, room)
			}
		}
	}

	engine := fixedpoint.New(g.Decimals, nil, "", "")
	solution := &Solution{
		SourceID:   sourceID,
		ListenerID: listenerID,
		Rooms:      rooms,
		Doors:      doors,
		Points:     points,
		LengthQ:    new(big.Int).Set(dist[target]),
		Length:     engine.Float(dist[target]),
	}
	solution.WallClear = PolylineClear(g.Level, solution.Points)
	return solution, nil
}

func PolylineClear(level *model.Level, points []model.Point) bool {
	if len(points) < 2 {
		return true
	}
	doorsByWall := map[string][]model.Door{}
	for _, door := range level.Doors {
		doorsByWall[door.WallID] = append(doorsByWall[door.WallID], door)
	}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		for _, wall := range level.Walls {
			if !geom.SegmentsIntersect(a, b, wall.A, wall.B) {
				continue
			}
			if geom.Cross(wall.A, wall.B, a) == 0 && geom.Cross(wall.A, wall.B, b) == 0 {
				continue
			}
			if pointAtDoor(a, wall, doorsByWall[wall.ID]) || pointAtDoor(b, wall, doorsByWall[wall.ID]) {
				continue
			}
			return false
		}
	}
	return true
}

func pointAtDoor(p model.Point, wall model.Wall, doors []model.Door) bool {
	if !geom.OnSegment(p, wall.A, wall.B) {
		return false
	}
	for _, door := range doors {
		if geom.OnSegment(p, door.A, door.B) {
			return true
		}
	}
	return false
}
