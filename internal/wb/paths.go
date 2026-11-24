package wb

import (
	"fmt"
	"sort"
	"strings"
)

type GraphEdge struct {
	Other   string
	Opening Opening
}

type Graph struct {
	Adj map[string][]GraphEdge
}

func BuildGraph(s *Sample) *Graph {
	g := &Graph{Adj: map[string][]GraphEdge{}}
	for _, room := range s.Rooms {
		g.Adj[room.ID] = nil
	}
	for _, o := range s.Openings {
		if o.RoomA == "" || o.RoomB == "" {
			continue
		}
		g.Adj[o.RoomA] = append(g.Adj[o.RoomA], GraphEdge{Other: o.RoomB, Opening: o})
		g.Adj[o.RoomB] = append(g.Adj[o.RoomB], GraphEdge{Other: o.RoomA, Opening: o})
	}
	for id := range g.Adj {
		edges := g.Adj[id]
		sort.Slice(edges, func(i, j int) bool { return edges[i].Opening.ID < edges[j].Opening.ID })
		g.Adj[id] = edges
	}
	return g
}

func FindSource(s *Sample, id string) (Source, bool) {
	for _, src := range s.Sources {
		if src.ID == id {
			return src, true
		}
	}
	return Source{}, false
}

func FindListener(s *Sample, id string) (Listener, bool) {
	for _, l := range s.Listeners {
		if l.ID == id {
			return l, true
		}
	}
	return Listener{}, false
}

func FindRoom(s *Sample, id string) (Room, bool) {
	for _, r := range s.Rooms {
		if r.ID == id {
			return r, true
		}
	}
	return Room{}, false
}

type candidate struct {
	rooms    []string
	openings []Opening
	points   []Point
	length   int
	polyline []Point
}

func SolvePairPath(s *Sample, g *Graph, sourceID, listenerID string) (*PathResult, *APIError) {
	src, ok := FindSource(s, sourceID)
	if !ok {
		return nil, NewAPIError(CodeSchema, sourceID, "sourceId", "unknown source")
	}
	listener, ok := FindListener(s, listenerID)
	if !ok {
		return nil, NewAPIError(CodeSchema, listenerID, "listenerId", "unknown listener")
	}
	sourcePoint := Point{X: src.X, Y: src.Y}
	listenerPoint := Point{X: listener.X, Y: listener.Y}
	sourceRoom, apiErr := RoomAtPoint(s, sourcePoint, src.ID, "source")
	if apiErr != nil {
		return nil, apiErr
	}
	listenerRoom, apiErr := RoomAtPoint(s, listenerPoint, listener.ID, "listener")
	if apiErr != nil {
		return nil, apiErr
	}

	var best *candidate
	consider := func(rooms []string, openings []Opening) {
		points := []Point{sourcePoint}
		length := 0
		prev := sourcePoint
		for _, o := range openings {
			mid := o.Midpoint()
			points = append(points, mid)
			length += manhattan(prev, mid)
			prev = mid
		}
		points = append(points, listenerPoint)
		length += manhattan(prev, listenerPoint)

		polyline, apiErr := buildRoomConstrainedPolyline(s, rooms, points)
		if apiErr != nil {
			return
		}
		cand := &candidate{
			rooms:    append([]string(nil), rooms...),
			openings: append([]Opening(nil), openings...),
			points:   points,
			length:   length,
			polyline: polyline,
		}
		if best == nil || betterCandidate(cand, best) {
			best = cand
		}
	}

	if sourceRoom == listenerRoom {
		consider([]string{sourceRoom}, nil)
	} else {
		visited := map[string]bool{sourceRoom: true}
		var dfs func(room string, rooms []string, openings []Opening)
		dfs = func(room string, rooms []string, openings []Opening) {
			for _, edge := range g.Adj[room] {
				if visited[edge.Other] {
					continue
				}
				nextRooms := append(append([]string(nil), rooms...), edge.Other)
				nextOpenings := append(append([]Opening(nil), openings...), edge.Opening)
				if edge.Other == listenerRoom {
					consider(nextRooms, nextOpenings)
					continue
				}
				if len(nextRooms) >= len(s.Rooms) {
					continue
				}
				visited[edge.Other] = true
				dfs(edge.Other, nextRooms, nextOpenings)
				delete(visited, edge.Other)
			}
		}
		dfs(sourceRoom, []string{sourceRoom}, nil)
	}

	if best == nil {
		apiErr := NewAPIError(CodeConn, sourceID+"/"+listenerID, "",
			"no door-connected room chain from source %s to listener %s", sourceID, listenerID)
		apiErr.Details = map[string]string{"sourceId": sourceID, "listenerId": listenerID}
		return nil, apiErr
	}

	doorIDs := make([]string, 0, len(best.openings))
	for _, o := range best.openings {
		doorIDs = append(doorIDs, o.ID)
	}
	occPoints, occDB := ComputeOcclusion(s, sourcePoint, listenerPoint)
	return &PathResult{
		SourceID:        sourceID,
		ListenerID:      listenerID,
		SourceRoom:      sourceRoom,
		ListenerRoom:    listenerRoom,
		RoomChain:       best.rooms,
		DoorChain:       doorIDs,
		Length:          best.length,
		Polyline:        best.polyline,
		OcclusionCount:  len(occPoints),
		OcclusionPoints: occPoints,
		OcclusionDB:     occDB,
	}, nil
}

func betterCandidate(a, b *candidate) bool {
	if len(a.openings) != len(b.openings) {
		return len(a.openings) < len(b.openings)
	}
	if a.length != b.length {
		return a.length < b.length
	}
	return doorChainKey(a.openings) < doorChainKey(b.openings)
}

func doorChainKey(openings []Opening) string {
	ids := make([]string, 0, len(openings))
	for _, o := range openings {
		ids = append(ids, o.ID)
	}
	return strings.Join(ids, ">")
}

func buildRoomConstrainedPolyline(s *Sample, rooms []string, points []Point) ([]Point, *APIError) {
	if len(rooms)+1 != len(points) {
		return nil, NewAPIError(CodeInternal, "path", "", "path point/room alignment error")
	}
	result := []Point{points[0]}
	for i := 0; i+1 < len(points); i++ {
		roomID := rooms[i]
		_, ok := FindRoom(s, roomID)
		if !ok {
			return nil, NewAPIError(CodeInternal, roomID, "", "path references missing room")
		}
		a := points[i]
		b := points[i+1]
		bends := []Point{{X: a.X, Y: b.Y}, {X: b.X, Y: a.Y}}
		var chosen []Point
		for _, bend := range bends {
			candidate := []Point{a}
			if bend != a && bend != b {
				candidate = append(candidate, bend)
			}
			candidate = append(candidate, b)
			hit, wallID, crossed := polylineCrossesWall(candidate, s.Walls, s.Openings)
			if !crossed {
				chosen = candidate
				break
			}
			_ = hit
			_ = wallID
		}
		if chosen == nil {
			return nil, NewAPIError(CodeConn, roomID, "", "path polyline crosses a wall in room %s", roomID)
		}
		for _, p := range chosen[1:] {
			if len(result) == 0 || result[len(result)-1] != p {
				result = append(result, p)
			}
		}
	}
	if hit, wallID, crossed := polylineCrossesWall(result, s.Walls, s.Openings); crossed {
		return nil, NewAPIError(CodeConn, wallID, "", "result polyline crosses wall near %v", hit)
	}
	return result, nil
}

func ComputeOcclusion(s *Sample, a, b Point) ([]Point, Fixed) {
	unique := map[string]bool{}
	points := []Point{}
	for _, w := range s.Walls {
		if hit, ok := segmentHitsWallOutsideOpening(a, b, w, s.Openings); ok {
			key := fmt.Sprintf("%d,%d,%s", hit.X, hit.Y, w.ID)
			if !unique[key] {
				unique[key] = true
				points = append(points, hit)
			}
		}
	}
	return points, Fixed(len(points)) * s.Precision.OcclusionAttenDB
}
