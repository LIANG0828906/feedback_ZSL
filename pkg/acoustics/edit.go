package acoustics

import (
	"encoding/json"
	"sort"
)

// Edit is the serializable local preview/edit command.
type Edit struct {
	Type       string   `json:"type"`
	ObjectID   string   `json:"object_id,omitempty"`
	RoomID     string   `json:"room_id,omitempty"`
	Side       string   `json:"side,omitempty"`
	WallID     string   `json:"wall_id,omitempty"`
	Coordinate float64  `json:"coordinate,omitempty"`
	Absorption *float64 `json:"absorption,omitempty"`
	Door       *Door    `json:"door,omitempty"`
	Open       *bool    `json:"open,omitempty"`
}

func CloneLevel(in *Level) (*Level, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out Level
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func ApplyEdit(in *Level, edit Edit) (*Level, error) {
	l, err := CloneLevel(in)
	if err != nil {
		return nil, err
	}
	switch edit.Type {
	case "set_absorption":
		l, err = SetAbsorption(l, edit.ObjectID, derefFloat(edit.Absorption))
	case "move_wall":
		l, err = MoveSharedWall(l, edit.WallID, edit.Coordinate)
	case "drag_room_boundary":
		l, err = DragRoomBoundary(l, edit.RoomID, edit.Side, edit.Coordinate)
	case "add_door":
		if edit.Door == nil {
			return nil, Errorf(CodeSchema, "edit", "door", "add_door requires door")
		}
		l, err = AddDoor(l, *edit.Door)
	case "delete_door":
		l, err = DeleteDoor(l, edit.ObjectID)
	case "set_door_open":
		l, err = SetDoorOpen(l, edit.ObjectID, derefBool(edit.Open))
	default:
		return nil, Errorf(CodeSchema, edit.Type, "type", "unknown edit type")
	}
	if err != nil {
		return nil, err
	}
	if err := Validate(l); err != nil {
		return nil, err
	}
	return l, nil
}

func SetAbsorption(l *Level, reflectorID string, alpha float64) (*Level, error) {
	for i := range l.Reflectors {
		if l.Reflectors[i].ID == reflectorID {
			l.Reflectors[i].Absorption = alpha
			return l, nil
		}
	}
	return nil, Errorf(CodeSchema, reflectorID, "object_id", "reflector not found")
}

func AddDoor(l *Level, d Door) (*Level, error) {
	for _, existing := range l.Doors {
		if existing.ID == d.ID {
			return nil, Errorf(CodeSchema, d.ID, "id", "door already exists")
		}
	}
	l.Doors = append(l.Doors, d)
	return l, nil
}

func DeleteDoor(l *Level, doorID string) (*Level, error) {
	out := l.Doors[:0]
	found := false
	for _, d := range l.Doors {
		if d.ID == doorID {
			found = true
			continue
		}
		out = append(out, d)
	}
	l.Doors = out
	if !found {
		return nil, Errorf(CodeSchema, doorID, "object_id", "door not found")
	}
	return l, nil
}

func SetDoorOpen(l *Level, doorID string, open bool) (*Level, error) {
	for i := range l.Doors {
		if l.Doors[i].ID == doorID {
			b := open
			l.Doors[i].Open = &b
			return l, nil
		}
	}
	return nil, Errorf(CodeSchema, doorID, "object_id", "door not found")
}

func MoveSharedWall(l *Level, wallID string, coordinate float64) (*Level, error) {
	wi := -1
	for i := range l.Walls {
		if l.Walls[i].ID == wallID {
			wi = i
			break
		}
	}
	if wi < 0 {
		return nil, Errorf(CodeSchema, wallID, "wall_id", "wall not found")
	}
	w := &l.Walls[wi]
	vertical := almostEqual(w.A.X, w.B.X)
	old := w.A.X
	if !vertical {
		old = w.A.Y
	}
	if vertical {
		w.A.X, w.B.X = coordinate, coordinate
	} else {
		w.A.Y, w.B.Y = coordinate, coordinate
	}

	moveRectSide := func(roomID string) {
		for ri := range l.Rooms {
			r := &l.Rooms[ri]
			if r.ID != roomID {
				continue
			}
			if vertical {
				if almostEqual(r.Rect.X2, old) {
					r.Rect.X2 = coordinate
				} else if almostEqual(r.Rect.X1, old) {
					r.Rect.X1 = coordinate
				}
			} else {
				if almostEqual(r.Rect.Y2, old) {
					r.Rect.Y2 = coordinate
				} else if almostEqual(r.Rect.Y1, old) {
					r.Rect.Y1 = coordinate
				}
			}
		}
	}
	moveRectSide(w.RoomA)
	moveRectSide(w.RoomB)

	for di := range l.Doors {
		d := &l.Doors[di]
		if d.WallID != wallID {
			continue
		}
		if vertical {
			d.Center.X = coordinate
		} else {
			d.Center.Y = coordinate
		}
	}
	for ri := range l.Reflectors {
		r := &l.Reflectors[ri]
		if vertical {
			if almostEqual(r.A.X, old) {
				r.A.X = coordinate
			}
			if almostEqual(r.B.X, old) {
				r.B.X = coordinate
			}
		} else {
			if almostEqual(r.A.Y, old) {
				r.A.Y = coordinate
			}
			if almostEqual(r.B.Y, old) {
				r.B.Y = coordinate
			}
		}
	}
	return l, nil
}

func DragRoomBoundary(l *Level, roomID, side string, coordinate float64) (*Level, error) {
	room, ok := roomByID(l, roomID)
	if !ok {
		return nil, Errorf(CodeSchema, roomID, "room_id", "room not found")
	}
	vertical := side == "left" || side == "right"
	old := room.Rect.X1
	if side == "right" {
		old = room.Rect.X2
	}
	if !vertical {
		old = room.Rect.Y1
		if side == "top" {
			old = room.Rect.Y2
		}
	}
	for _, w := range l.Walls {
		if w.RoomA != roomID && w.RoomB != roomID {
			continue
		}
		if vertical && almostEqual(w.A.X, w.B.X) && almostEqual(w.A.X, old) {
			return MoveSharedWall(l, w.ID, coordinate)
		}
		if !vertical && almostEqual(w.A.Y, w.B.Y) && almostEqual(w.A.Y, old) {
			return MoveSharedWall(l, w.ID, coordinate)
		}
	}
	for i := range l.Rooms {
		if l.Rooms[i].ID != roomID {
			continue
		}
		switch side {
		case "left":
			l.Rooms[i].Rect.X1 = coordinate
		case "right":
			l.Rooms[i].Rect.X2 = coordinate
		case "bottom":
			l.Rooms[i].Rect.Y1 = coordinate
		case "top":
			l.Rooms[i].Rect.Y2 = coordinate
		default:
			return nil, Errorf(CodeSchema, side, "side", "side must be left/right/bottom/top")
		}
		return l, nil
	}
	return nil, Errorf(CodeSchema, roomID, "room_id", "room not found after boundary edit")
}

func roomByID(l *Level, id string) (Room, bool) {
	for _, r := range l.Rooms {
		if r.ID == id {
			return r, true
		}
	}
	return Room{}, false
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

type chainSignature struct {
	Code        string
	RoomIDs     []string
	DoorIDs     []string
	LengthFixed int64
}

func solveSignature(l *Level, src Source, lis Listener) chainSignature {
	idx := NewIndex(l)
	c, err := idx.SolvePointChain(src.ID, lis.ID, src.Position, src.RoomID, lis.Position, lis.RoomID)
	if err != nil {
		if de, ok := AsDomainError(err); ok {
			return chainSignature{Code: de.Code}
		}
		return chainSignature{Code: "E_ERROR"}
	}
	return chainSignature{RoomIDs: c.RoomIDs, DoorIDs: c.DoorIDs, LengthFixed: c.LengthFixed}
}

// PlanInvalidation compares two revisions and previous results. It never
// mutates either level or report.
func PlanInvalidation(before, after *Level, previous *Report) *InvalidationReport {
	rep := &InvalidationReport{Reason: "dependency-aware edit diff"}
	seen := map[string]bool{}
	invalidRooms := map[string]bool{}

	appendItem := func(roomID, objectID, kind, reason string) {
		key := roomID + "\x00" + objectID + "\x00" + kind
		if seen[key] {
			return
		}
		seen[key] = true
		item := InvalidationItem{RoomID: roomID, ObjectID: objectID, Kind: kind, Reason: reason}
		rep.Items = append(rep.Items, item)
		if roomID != "" {
			invalidRooms[roomID] = true
		}
	}

	beforeRef := map[string]Reflector{}
	afterRef := map[string]Reflector{}
	for _, r := range before.Reflectors {
		beforeRef[r.ID] = r
	}
	for _, r := range after.Reflectors {
		afterRef[r.ID] = r
	}
	for id, nr := range afterRef {
		or, ok := beforeRef[id]
		if !ok {
			appendItem(nr.RoomID, id, "surface", "reflector added")
			invalidateListenersForSurface(previous, nr.RoomID, id, appendItem)
			continue
		}
		if HashCanonical(or) != HashCanonical(nr) {
			appendItem(nr.RoomID, id, "surface", "reflector parameters or geometry changed")
			invalidateListenersForSurface(previous, nr.RoomID, id, appendItem)
		}
	}
	for id, or := range beforeRef {
		if _, ok := afterRef[id]; !ok {
			appendItem(or.RoomID, id, "surface", "reflector deleted")
			invalidateListenersForSurface(previous, or.RoomID, id, appendItem)
		}
	}

	beforeDoor := map[string]Door{}
	afterDoor := map[string]Door{}
	wallByID := map[string]Wall{}
	for _, w := range after.Walls {
		wallByID[w.ID] = w
	}
	for _, w := range before.Walls {
		if _, ok := wallByID[w.ID]; !ok {
			wallByID[w.ID] = w
		}
	}
	for _, d := range before.Doors {
		beforeDoor[d.ID] = d
	}
	for _, d := range after.Doors {
		afterDoor[d.ID] = d
	}
	doorRooms := func(d Door) []string {
		w, ok := wallByID[d.WallID]
		if !ok {
			return nil
		}
		return []string{w.RoomA, w.RoomB}
	}
	for id, nd := range afterDoor {
		rooms := doorRooms(nd)
		od, ok := beforeDoor[id]
		if !ok {
			for _, room := range rooms {
				appendItem(room, id, "door", "door opening/connectivity added")
			}
			continue
		}
		if HashCanonical(od) != HashCanonical(nd) {
			for _, room := range rooms {
				appendItem(room, id, "door", "door snapshot or opening relation changed")
			}
		}
	}
	for id, od := range beforeDoor {
		if _, ok := afterDoor[id]; !ok {
			for _, room := range doorRooms(od) {
				appendItem(room, id, "door", "door opening/connectivity deleted")
			}
		}
	}

	for _, nr := range after.Rooms {
		or, ok := roomByID(before, nr.ID)
		if !ok {
			appendItem(nr.ID, nr.ID, "room", "room added")
			continue
		}
		if HashCanonical(or) != HashCanonical(nr) {
			appendItem(nr.ID, nr.ID, "room_snapshot", "room boundary/content hash changed")
		}
	}

	pairResults := map[string]PairResult{}
	if previous != nil {
		for _, pr := range previous.Pairs {
			pairResults[pr.SourceID+"\x00"+pr.ListenerID] = pr
		}
	}

	sources := append([]Source{}, after.Sources...)
	listeners := append([]Listener{}, after.Listeners...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].ID < listeners[j].ID })
	for _, lis := range listeners {
		for _, src := range sources {
			beforeSig := solveSignature(before, src, lis)
			afterSig := solveSignature(after, src, lis)
			if HashCanonical(beforeSig) == HashCanonical(afterSig) {
				continue
			}
			pairID := "chain:" + src.ID + "->" + lis.ID
			reason := chainDiffReason(beforeSig, afterSig)
			rooms := map[string]bool{}
			for _, r := range beforeSig.RoomIDs {
				rooms[r] = true
			}
			for _, r := range afterSig.RoomIDs {
				rooms[r] = true
			}
			roomList := sortedStringSet(rooms)
			for _, room := range roomList {
				appendItem(room, pairID, "chain", reason)
			}
			appendItem(lis.RoomID, lis.ID, "listener", "downstream listener solve invalidated: "+reason)
			if pr, ok := pairResults[src.ID+"\x00"+lis.ID]; ok {
				_ = pr
			}
		}
	}

	for room := range invalidRooms {
		rep.InvalidRooms = append(rep.InvalidRooms, room)
		rep.RecomputedRooms = append(rep.RecomputedRooms, room)
	}
	sort.Strings(rep.InvalidRooms)
	sort.Strings(rep.RecomputedRooms)
	for _, r := range after.Rooms {
		if !invalidRooms[r.ID] {
			rep.ReusedRooms = append(rep.ReusedRooms, r.ID)
		}
	}
	sort.Slice(rep.Items, func(i, j int) bool {
		if rep.Items[i].RoomID != rep.Items[j].RoomID {
			return rep.Items[i].RoomID < rep.Items[j].RoomID
		}
		if rep.Items[i].ObjectID != rep.Items[j].ObjectID {
			return rep.Items[i].ObjectID < rep.Items[j].ObjectID
		}
		return rep.Items[i].Kind < rep.Items[j].Kind
	})
	return rep
}

func invalidateListenersForSurface(previous *Report, roomID, reflectorID string, add func(roomID, objectID, kind, reason string)) {
	if previous == nil {
		add(roomID, "listeners:"+roomID, "listener", "surface changed; no previous report, invalidate all room listeners")
		return
	}
	for _, pr := range previous.Pairs {
		if pr.ListenerRoomID != roomID {
			continue
		}
		for _, h := range pr.Reflections {
			if h.ReflectorID == reflectorID {
				add(roomID, pr.ListenerID, "listener", "reflection path "+reflectorID+" affects pair "+pr.SourceID+"->"+pr.ListenerID)
				add(roomID, pr.SourceID+"->"+pr.ListenerID, "pair", "reflector parameter/geometry changed")
				break
			}
		}
	}
}

func chainDiffReason(a, b chainSignature) string {
	if a.Code != b.Code {
		return "connectivity status changed " + a.Code + " -> " + b.Code
	}
	if stringsJoin(a.RoomIDs) != stringsJoin(b.RoomIDs) || stringsJoin(a.DoorIDs) != stringsJoin(b.DoorIDs) {
		return "room/door chain changed"
	}
	if a.LengthFixed != b.LengthFixed {
		return "fixed-point chain length changed"
	}
	return "chain signature changed"
}

func sortedStringSet(set map[string]bool) []string {
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func stringsJoin(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += "|"
		}
		out += x
	}
	return out
}
