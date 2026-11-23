package solver

import (
	"container/heap"
	"fmt"

	"roomreverb/internal/errs"
	"roomreverb/internal/fixed"
	"roomreverb/internal/model"
)

type FoundPath struct {
	Rooms        []string
	Portals      []string
	Length       fixed.Value
	SourceRoom   string
	ListenerRoom string
}

type pathState struct {
	room    string
	entry   string
	pos     model.Point
	cost    fixed.Value
	rooms   []string
	portals []string
	serial  int64
}

type pathHeap []*pathState

func (h pathHeap) Len() int { return len(h) }
func (h pathHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if a.room != b.room {
		return a.room < b.room
	}
	if a.entry != b.entry {
		return a.entry < b.entry
	}
	return a.serial < b.serial
}
func (h pathHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pathHeap) Push(x any)   { *h = append(*h, x.(*pathState)) }
func (h *pathHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

func stateKey(room, entry string) string { return room + "\x00" + entry }

// ShortestPath finds the shortest portal-connected path for one source/listener
// pair. The graph nodes are rooms; the search state additionally remembers the
// entry portal because portal-to-portal distance depends on where the path
// entered the room. All arithmetic is Q16.16.
func ShortestPath(g *Graph, sourceID, listenerID string, pathBufferLimit int) (FoundPath, error) {
	if pathBufferLimit <= 0 {
		pathBufferLimit = DefaultPathBufferLimit
	}
	src, ok := g.Level.Source(sourceID)
	if !ok {
		return FoundPath{}, errs.New(errs.EInput, "unknown source "+sourceID)
	}
	lst, ok := g.Level.Listener(listenerID)
	if !ok {
		return FoundPath{}, errs.New(errs.EInput, "unknown listener "+listenerID)
	}
	srcRoom, ok := g.RoomOfPoint(src.Pos)
	if !ok {
		return FoundPath{}, &errs.Error{Code: errs.ERoom, Source: sourceID, Field: "pos", Message: "source is outside all rooms"}
	}
	lstRoom, ok := g.RoomOfPoint(lst.Pos)
	if !ok {
		return FoundPath{}, &errs.Error{Code: errs.ERoom, Listener: listenerID, Field: "pos", Message: "listener is outside all rooms"}
	}
	if srcRoom == lstRoom {
		d := fixed.Distance(src.Pos.X, src.Pos.Y, lst.Pos.X, lst.Pos.Y)
		return FoundPath{
			Rooms: []string{srcRoom}, Portals: []string{}, Length: d,
			SourceRoom: srcRoom, ListenerRoom: lstRoom,
		}, nil
	}

	start := &pathState{
		room: srcRoom, entry: "", pos: src.Pos, cost: 0,
		rooms: []string{srcRoom},
	}
	q := &pathHeap{start}
	heap.Init(q)
	best := map[string]fixed.Value{stateKey(srcRoom, ""): 0}
	var bestPath *FoundPath
	var serial int64
	for q.Len() > 0 {
		cur := heap.Pop(q).(*pathState)
		if bestPath != nil && cur.cost > bestPath.Length {
			break
		}
		if v, ok := best[stateKey(cur.room, cur.entry)]; !ok || cur.cost > v {
			continue
		}
		if cur.room == lstRoom {
			total := cur.cost + fixed.Distance(cur.pos.X, cur.pos.Y, lst.Pos.X, lst.Pos.Y)
			if bestPath == nil || total < bestPath.Length ||
				(total == bestPath.Length && lexLess(cur.rooms, cur.portals, bestPath.Rooms, bestPath.Portals)) {
				bestPath = &FoundPath{
					Rooms:   append([]string(nil), cur.rooms...),
					Portals: append([]string(nil), cur.portals...),
					Length:  total, SourceRoom: srcRoom, ListenerRoom: lstRoom,
				}
			}
			continue
		}
		edges := g.Adj[cur.room]
		if len(edges) == 0 {
			continue
		}
		for _, e := range edges {
			if e.ID == cur.entry {
				continue
			}
			nextRoom := e.Other(cur.room)
			step := fixed.Distance(cur.pos.X, cur.pos.Y, e.Center.X, e.Center.Y)
			nextCost := cur.cost + step
			if bestPath != nil && nextCost > bestPath.Length {
				continue
			}
			key := stateKey(nextRoom, e.ID)
			if old, ok := best[key]; ok && old <= nextCost {
				continue
			}
			if len(cur.rooms)+1 > pathBufferLimit {
				return FoundPath{}, &errs.Error{
					Code: errs.EResource, Listener: listenerID, Source: sourceID,
					Field:   "path_buffer_limit",
					Message: fmt.Sprintf("path exceeds per-thread buffer limit %d", pathBufferLimit),
				}
			}
			best[key] = nextCost
			serial++
			rooms := append(append([]string(nil), cur.rooms...), nextRoom)
			portals := append(append([]string(nil), cur.portals...), e.ID)
			heap.Push(q, &pathState{
				room: nextRoom, entry: e.ID, pos: e.Center, cost: nextCost,
				rooms: rooms, portals: portals, serial: serial,
			})
		}
	}
	if bestPath == nil {
		return FoundPath{}, &errs.Error{
			Code: errs.EConn, Source: sourceID, Listener: listenerID,
			Field:   "room_path",
			Message: fmt.Sprintf("source %s in room %s cannot reach listener %s in room %s through active openings", sourceID, srcRoom, listenerID, lstRoom),
		}
	}
	return *bestPath, nil
}

func lexLess(roomsA, portalsA, roomsB, portalsB []string) bool {
	n := len(roomsA)
	if len(roomsB) < n {
		n = len(roomsB)
	}
	for i := 0; i < n; i++ {
		if roomsA[i] != roomsB[i] {
			return roomsA[i] < roomsB[i]
		}
	}
	n = len(portalsA)
	if len(portalsB) < n {
		n = len(portalsB)
	}
	for i := 0; i < n; i++ {
		if portalsA[i] != portalsB[i] {
			return portalsA[i] < portalsB[i]
		}
	}
	return len(roomsA) < len(roomsB)
}
