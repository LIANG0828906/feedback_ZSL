package geom

import (
	"fmt"
	"sort"
	"strings"

	"levelacoustic/internal/fixed"
	"levelacoustic/internal/model"
)

type DoorLink struct {
	Door   model.Door
	Wall   model.Wall
	Center model.Point
	RoomA  string
	RoomB  string
}

func (d DoorLink) OtherRoom(roomID string) (string, bool) {
	switch roomID {
	case d.RoomA:
		return d.RoomB, true
	case d.RoomB:
		return d.RoomA, true
	default:
		return "", false
	}
}

type Graph struct {
	Project   *model.Project
	Rooms     map[string]model.Room
	Walls     map[string]model.Wall
	Doors     map[string]DoorLink
	RoomDoors map[string][]string
	DoorOrder []string
}

func Build(p *model.Project) (*Graph, error) {
	g := &Graph{
		Project:   p,
		Rooms:     make(map[string]model.Room, len(p.Rooms)),
		Walls:     make(map[string]model.Wall, len(p.Walls)),
		Doors:     make(map[string]DoorLink, len(p.Doors)),
		RoomDoors: make(map[string][]string),
		DoorOrder: make([]string, 0, len(p.Doors)),
	}
	for _, room := range p.Rooms {
		g.Rooms[room.ID] = room
	}
	for _, wall := range p.Walls {
		g.Walls[wall.ID] = wall
	}
	for _, door := range p.Doors {
		wall, ok := g.Walls[door.WallID]
		if !ok {
			return nil, model.GeometryError("", door.WallID, "wall_id", "door %s references unknown wall %s", door.ID, door.WallID)
		}
		link := DoorLink{
			Door:   door,
			Wall:   wall,
			Center: model.DoorCenter(wall, door),
			RoomA:  wall.RoomA,
			RoomB:  wall.RoomB,
		}
		g.Doors[door.ID] = link
		g.RoomDoors[wall.RoomA] = append(g.RoomDoors[wall.RoomA], door.ID)
		g.RoomDoors[wall.RoomB] = append(g.RoomDoors[wall.RoomB], door.ID)
		g.DoorOrder = append(g.DoorOrder, door.ID)
	}
	sort.Strings(g.DoorOrder)
	for roomID := range g.RoomDoors {
		sort.Strings(g.RoomDoors[roomID])
	}
	return g, nil
}

func (g *Graph) RoomForPoint(x, y int) (string, bool) {
	for _, room := range g.Project.Rooms {
		if room.Rect.ContainsInterior(x, y) {
			return room.ID, true
		}
	}
	return "", false
}

type Path struct {
	Reachable       bool
	ListenerRoomID  string
	RoomChain       []string
	DoorChain       []string
	PathLengthMicro int64
	Segments        []model.TruthSegment
	Truncations     []model.Truncation
}

func DirectPath(g *Graph, source, listener model.Point) (Path, error) {
	sourceRoom, ok := g.RoomForPoint(source.X, source.Y)
	if !ok {
		return Path{}, model.GeometryError("", "", "source_position", "source %s is outside all rooms", source.ID)
	}
	listenerRoom, ok := g.RoomForPoint(listener.X, listener.Y)
	if !ok {
		return Path{}, model.GeometryError("", "", "listener_position", "listener %s is outside all rooms", listener.ID)
	}
	if sourceRoom != listenerRoom {
		return Path{}, fmt.Errorf("direct path requested across rooms %s and %s", sourceRoom, listenerRoom)
	}
	length, rem := fixed.SegmentLength(int64(listener.X-source.X), int64(listener.Y-source.Y))
	return Path{
		Reachable:       true,
		ListenerRoomID:  listenerRoom,
		RoomChain:       []string{sourceRoom},
		PathLengthMicro: length,
		Segments: []model.TruthSegment{{
			RoomID:      sourceRoom,
			From:        source.ID,
			To:          listener.ID,
			LengthMicro: length,
		}},
		Truncations: []model.Truncation{{
			Stage:     "path_segment",
			Step:      0,
			Remainder: rem,
			Fixed:     length,
		}},
	}, nil
}

func BuildPath(g *Graph, source, listener model.Point, doorChain []string) (Path, error) {
	sourceRoom, ok := g.RoomForPoint(source.X, source.Y)
	if !ok {
		return Path{}, model.GeometryError("", "", "source_position", "source %s is outside all rooms", source.ID)
	}
	listenerRoom, ok := g.RoomForPoint(listener.X, listener.Y)
	if !ok {
		return Path{}, model.GeometryError("", "", "listener_position", "listener %s is outside all rooms", listener.ID)
	}
	if len(doorChain) == 0 {
		return DirectPath(g, source, listener)
	}

	currentRoom := sourceRoom
	points := make([]model.Point, 0, len(doorChain)+2)
	points = append(points, source)
	rooms := []string{sourceRoom}
	seenDoors := make(map[string]struct{}, len(doorChain))
	for i, doorID := range doorChain {
		link, ok := g.Doors[doorID]
		if !ok {
			return Path{}, model.GeometryError("", "", "door_chain", "unknown door %s at step %d", doorID, i)
		}
		if _, seen := seenDoors[doorID]; seen {
			return Path{}, model.GeometryError("", link.Wall.ID, "door_chain", "door %s is repeated", doorID)
		}
		seenDoors[doorID] = struct{}{}
		nextRoom, ok := link.OtherRoom(currentRoom)
		if !ok {
			return Path{}, model.GeometryError("", link.Wall.ID, "door_chain", "door %s is not incident to room %s", doorID, currentRoom)
		}
		points = append(points, link.Center)
		currentRoom = nextRoom
		rooms = append(rooms, currentRoom)
	}
	if currentRoom != listenerRoom {
		return Path{}, model.GeometryError(listenerRoom, "", "door_chain", "door chain ends in room %s, listener %s is in %s", currentRoom, listener.ID, listenerRoom)
	}
	points = append(points, listener)

	segments := make([]model.TruthSegment, 0, len(rooms))
	truncations := make([]model.Truncation, 0, len(rooms))
	var total int64
	for i, roomID := range rooms {
		room := g.Rooms[roomID]
		a, b := points[i], points[i+1]
		if !segmentInsideRect(room.Rect, a, b) {
			return Path{}, model.GeometryError(roomID, "", "polyline", "segment %d from %s to %s leaves room %s", i, a.ID, b.ID, roomID)
		}
		length, rem := fixed.SegmentLength(int64(b.X-a.X), int64(b.Y-a.Y))
		total += length
		segments = append(segments, model.TruthSegment{
			RoomID:      roomID,
			From:        a.ID,
			To:          b.ID,
			LengthMicro: length,
		})
		truncations = append(truncations, model.Truncation{
			Stage:     "path_segment",
			Step:      i,
			Remainder: rem,
			Fixed:     length,
		})
	}
	return Path{
		Reachable:       true,
		ListenerRoomID:  listenerRoom,
		RoomChain:       rooms,
		DoorChain:       append([]string(nil), doorChain...),
		PathLengthMicro: total,
		Segments:        segments,
		Truncations:     truncations,
	}, nil
}

// EnumerateTruthPaths enumerates every non-repeating door sequence. This is
// deliberately independent of the solver's Dijkstra state graph and is used
// only to construct generator truth.
func EnumerateTruthPaths(g *Graph, source, listener model.Point) (Path, error) {
	sourceRoom, ok := g.RoomForPoint(source.X, source.Y)
	if !ok {
		return Path{}, model.GeometryError("", "", "source_position", "source %s is outside all rooms", source.ID)
	}
	listenerRoom, ok := g.RoomForPoint(listener.X, listener.Y)
	if !ok {
		return Path{}, model.GeometryError("", "", "listener_position", "listener %s is outside all rooms", listener.ID)
	}
	if sourceRoom == listenerRoom {
		return DirectPath(g, source, listener)
	}

	var best *Path
	used := make(map[string]bool, len(g.Doors))
	var chain []string
	var dfs func(roomID string)
	dfs = func(roomID string) {
		if roomID == listenerRoom {
			p, err := BuildPath(g, source, listener, append([]string(nil), chain...))
			if err != nil {
				return
			}
			if best == nil || BetterPath(p, *best) {
				copyPath := p
				best = &copyPath
			}
			return
		}
		for _, doorID := range g.RoomDoors[roomID] {
			if used[doorID] {
				continue
			}
			link := g.Doors[doorID]
			next, ok := link.OtherRoom(roomID)
			if !ok {
				continue
			}
			used[doorID] = true
			chain = append(chain, doorID)
			dfs(next)
			chain = chain[:len(chain)-1]
			used[doorID] = false
		}
	}
	dfs(sourceRoom)
	if best == nil {
		return Path{Reachable: false, ListenerRoomID: listenerRoom}, nil
	}
	return *best, nil
}

func PathSortKey(p Path) string {
	return strings.Join(p.RoomChain, "\x00") + "\x01" + strings.Join(p.DoorChain, "\x00")
}

func BetterPath(a, b Path) bool {
	if a.PathLengthMicro != b.PathLengthMicro {
		return a.PathLengthMicro < b.PathLengthMicro
	}
	return PathSortKey(a) < PathSortKey(b)
}

func segmentInsideRect(r model.Rect, a, b model.Point) bool {
	return r.ContainsPoint(a.X, a.Y) && r.ContainsPoint(b.X, b.Y)
}

func ChainKey(rooms []string) string {
	return strings.Join(rooms, "\x00")
}

func DoorKey(doors []string) string {
	return strings.Join(doors, "\x00")
}
