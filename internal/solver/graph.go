package solver

import (
	"sort"

	"roomreverb/internal/model"
)

type PortalEdge struct {
	ID     string
	WallID string
	Rooms  [2]string
	Center model.Point
	Width  float64
}

func (e PortalEdge) Other(room string) string {
	if e.Rooms[0] == room {
		return e.Rooms[1]
	}
	return e.Rooms[0]
}

type Graph struct {
	Level   model.Level
	RoomOf  map[string]string
	Adj     map[string][]PortalEdge
	Active  []PortalEdge
	Portals []PortalRef
}

// BuildGraph constructs a deterministic room graph from active openings. Walls
// must have been validated beforehand.
func BuildGraph(level model.Level) *Graph {
	g := &Graph{
		Level:  level,
		RoomOf: make(map[string]string, len(level.Listeners)),
		Adj:    make(map[string][]PortalEdge, len(level.Rooms)),
	}
	for _, r := range level.Rooms {
		g.Adj[r.ID] = nil
	}
	for _, l := range level.Listeners {
		for _, r := range level.Rooms {
			if r.Rect.ContainsInclusive(l.Pos) {
				g.RoomOf[l.ID] = r.ID
				break
			}
		}
	}
	walls := make(map[string]model.Wall, len(level.Walls))
	for _, w := range level.Walls {
		walls[w.ID] = w
	}
	for _, o := range level.Openings {
		if !o.Active {
			continue
		}
		w, ok := walls[o.WallID]
		if !ok || len(w.Rooms) != 2 {
			continue
		}
		rooms := [2]string{w.Rooms[0], w.Rooms[1]}
		sort.Strings(rooms[:])
		e := PortalEdge{
			ID: o.ID, WallID: o.WallID, Rooms: rooms,
			Center: o.Center, Width: o.Width,
		}
		g.Adj[rooms[0]] = append(g.Adj[rooms[0]], e)
		g.Adj[rooms[1]] = append(g.Adj[rooms[1]], e)
		g.Active = append(g.Active, e)
		g.Portals = append(g.Portals, PortalRef{
			ID: o.ID, WallID: o.WallID,
			Rooms:  append([]string(nil), rooms[:]...),
			Center: o.Center, Width: o.Width,
		})
	}
	for room := range g.Adj {
		sort.Slice(g.Adj[room], func(i, j int) bool { return g.Adj[room][i].ID < g.Adj[room][j].ID })
	}
	sort.Slice(g.Active, func(i, j int) bool { return g.Active[i].ID < g.Active[j].ID })
	sort.Slice(g.Portals, func(i, j int) bool { return g.Portals[i].ID < g.Portals[j].ID })
	return g
}

func (g *Graph) Neighbors(room string) ([]string, []string) {
	roomSet := map[string]bool{}
	portals := make([]string, 0, len(g.Adj[room]))
	for _, e := range g.Adj[room] {
		roomSet[e.Other(room)] = true
		portals = append(portals, e.ID)
	}
	rooms := make([]string, 0, len(roomSet))
	for id := range roomSet {
		rooms = append(rooms, id)
	}
	sort.Strings(rooms)
	sort.Strings(portals)
	return rooms, portals
}

func (g *Graph) RoomOfPoint(p model.Point) (string, bool) {
	for _, r := range g.Level.Rooms {
		if r.Rect.ContainsInclusive(p) {
			return r.ID, true
		}
	}
	return "", false
}
