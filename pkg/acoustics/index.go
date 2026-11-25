package acoustics

import "sort"

// Index contains deterministic lookup tables for one validated Level.
type Index struct {
	Level          *Level
	RoomByID       map[string]Room
	WallByID       map[string]Wall
	DoorByID       map[string]Door
	DoorsByWall    map[string][]Door
	DoorsByRoom    map[string][]Door
	ReflectorsRoom map[string][]Reflector
	SourcesByID    map[string]Source
	ListenersByID  map[string]Listener
	RoomOrder      []string
}

func NewIndex(level *Level) *Index {
	idx := &Index{
		Level:          level,
		RoomByID:       map[string]Room{},
		WallByID:       map[string]Wall{},
		DoorByID:       map[string]Door{},
		DoorsByWall:    map[string][]Door{},
		DoorsByRoom:    map[string][]Door{},
		ReflectorsRoom: map[string][]Reflector{},
		SourcesByID:    map[string]Source{},
		ListenersByID:  map[string]Listener{},
	}
	for _, r := range level.Rooms {
		idx.RoomByID[r.ID] = r
		idx.RoomOrder = append(idx.RoomOrder, r.ID)
	}
	sort.Strings(idx.RoomOrder)
	for _, w := range level.Walls {
		idx.WallByID[w.ID] = w
	}
	for _, d := range level.Doors {
		idx.DoorByID[d.ID] = d
		idx.DoorsByWall[d.WallID] = append(idx.DoorsByWall[d.WallID], d)
		if w, ok := idx.WallByID[d.WallID]; ok && d.IsOpen() {
			idx.DoorsByRoom[w.RoomA] = append(idx.DoorsByRoom[w.RoomA], d)
			idx.DoorsByRoom[w.RoomB] = append(idx.DoorsByRoom[w.RoomB], d)
		}
	}
	for room := range idx.DoorsByRoom {
		sort.Slice(idx.DoorsByRoom[room], func(i, j int) bool { return idx.DoorsByRoom[room][i].ID < idx.DoorsByRoom[room][j].ID })
	}
	for wall := range idx.DoorsByWall {
		sort.Slice(idx.DoorsByWall[wall], func(i, j int) bool { return idx.DoorsByWall[wall][i].ID < idx.DoorsByWall[wall][j].ID })
	}
	for _, r := range level.Reflectors {
		idx.ReflectorsRoom[r.RoomID] = append(idx.ReflectorsRoom[r.RoomID], r)
	}
	for room := range idx.ReflectorsRoom {
		sort.Slice(idx.ReflectorsRoom[room], func(i, j int) bool { return idx.ReflectorsRoom[room][i].ID < idx.ReflectorsRoom[room][j].ID })
	}
	for _, s := range level.Sources {
		idx.SourcesByID[s.ID] = s
	}
	for _, l := range level.Listeners {
		idx.ListenersByID[l.ID] = l
	}
	return idx
}

// DoorsBetween returns open doors on a shared wall between two rooms.
func (idx *Index) DoorsBetween(a, b string) []Door {
	var out []Door
	for _, d := range idx.Level.Doors {
		if !d.IsOpen() {
			continue
		}
		w, ok := idx.WallByID[d.WallID]
		if !ok {
			continue
		}
		if (w.RoomA == a && w.RoomB == b) || (w.RoomA == b && w.RoomB == a) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
