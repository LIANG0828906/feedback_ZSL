package validate

import (
	"fmt"
	"math"
	"sort"

	"roomreverb/internal/errs"
	"roomreverb/internal/model"
)

const geomEpsilon = 1e-5

func Validate(level model.Level) error {
	if level.ID == "" {
		return errs.Decode("", "id", "level id is empty")
	}
	if len(level.Rooms) == 0 {
		return errs.Decode("", "rooms", "level has no rooms")
	}
	roomIDs := map[string]bool{}
	for _, r := range level.Rooms {
		if r.ID == "" {
			return errs.Decode("", "rooms.id", "room id is empty")
		}
		if roomIDs[r.ID] {
			return errs.Decode(r.ID, "id", "duplicate room id")
		}
		roomIDs[r.ID] = true
		if !finite(r.Rect.X) || !finite(r.Rect.Y) || !finite(r.Rect.W) || !finite(r.Rect.H) {
			return &errs.Error{Code: errs.ERoom, Room: r.ID, Field: "rect", Message: "room rectangle is not finite"}
		}
		if r.Rect.W <= 0 || r.Rect.H <= 0 {
			return &errs.Error{Code: errs.ERoom, Room: r.ID, Field: "rect", Message: "room width and height must be positive"}
		}
	}
	for i := 0; i < len(level.Rooms); i++ {
		for j := i + 1; j < len(level.Rooms); j++ {
			a, b := level.Rooms[i], level.Rooms[j]
			if rectsOverlap(a.Rect, b.Rect) {
				return &errs.Error{
					Code: errs.ERoom, Room: a.ID, Field: "rect",
					Message: fmt.Sprintf("room %s overlaps room %s", a.ID, b.ID),
				}
			}
		}
	}
	for _, r := range level.Rooms {
		if err := r.ValidateParams(); err != nil {
			field := "params"
			if r.Params.T60MS < 50 || r.Params.T60MS > 10000 {
				field = "t60_ms"
			} else if r.Params.EarlyDensity < 0 || r.Params.EarlyDensity > 1 {
				field = "early_density"
			} else if r.Params.ReverbGain < 0 || r.Params.ReverbGain > 4 {
				field = "reverb_gain"
			}
			return &errs.Error{Code: errs.EPara, Room: r.ID, Field: field, Message: err.Error()}
		}
	}

	wallIDs := map[string]model.Wall{}
	openingIDs := map[string]bool{}
	for _, w := range level.Walls {
		if w.ID == "" {
			return errs.Decode("", "walls.id", "wall id is empty")
		}
		if _, ok := wallIDs[w.ID]; ok {
			return errs.Decode("", "walls.id", "duplicate wall id %s", w.ID)
		}
		if len(w.Rooms) != 2 {
			return &errs.Error{Code: errs.ERoom, Room: firstRoom(w.Rooms), Field: "walls.rooms", Message: "shared wall must reference exactly two rooms"}
		}
		for _, id := range w.Rooms {
			if !roomIDs[id] {
				return &errs.Error{Code: errs.ERoom, Room: id, Field: "walls.rooms", Message: "wall references unknown room"}
			}
		}
		if !finite(w.Start.X) || !finite(w.Start.Y) || !finite(w.End.X) || !finite(w.End.Y) {
			return errs.Decode(firstRoom(w.Rooms), "walls."+w.ID, "wall endpoint is not finite")
		}
		dx, dy := w.End.X-w.Start.X, w.End.Y-w.Start.Y
		if math.Hypot(dx, dy) <= geomEpsilon {
			return &errs.Error{Code: errs.ERoom, Room: firstRoom(w.Rooms), Field: "walls." + w.ID, Message: "wall length must be positive"}
		}
		wallIDs[w.ID] = w
	}

	for _, o := range level.Openings {
		if o.ID == "" {
			return errs.Decode("", "openings.id", "opening id is empty")
		}
		if openingIDs[o.ID] {
			return errs.Decode("", "openings.id", "duplicate opening id %s", o.ID)
		}
		openingIDs[o.ID] = true
		w, ok := wallIDs[o.WallID]
		if !ok {
			return &errs.Error{Code: errs.ERoom, Room: firstRoom(w.Rooms), Field: "openings." + o.ID + ".wall_id", Message: "opening references unknown wall " + o.WallID}
		}
		if !finite(o.Center.X) || !finite(o.Center.Y) || !finite(o.Width) {
			return &errs.Error{Code: errs.ERoom, Room: firstRoom(w.Rooms), Field: "openings." + o.ID, Message: "opening values are not finite"}
		}
		if o.Width <= 0 {
			return &errs.Error{Code: errs.ERoom, Room: firstRoom(w.Rooms), Field: "openings." + o.ID + ".width", Message: "opening width must be positive"}
		}
		if !openingInsideWall(w, o) {
			return &errs.Error{Code: errs.ERoom, Room: firstRoom(w.Rooms), Field: "openings." + o.ID, Message: "opening does not lie inside its wall"}
		}
	}

	listenerIDs := map[string]bool{}
	for _, l := range level.Listeners {
		if l.ID == "" {
			return errs.Decode("", "listeners.id", "listener id is empty")
		}
		if listenerIDs[l.ID] {
			return errs.Decode("", "listeners.id", "duplicate listener id %s", l.ID)
		}
		listenerIDs[l.ID] = true
		if !finite(l.Pos.X) || !finite(l.Pos.Y) {
			return errs.Decode("", "listeners."+l.ID+".pos", "listener position is not finite")
		}
		rooms := roomsContaining(level, l.Pos)
		if len(rooms) != 1 {
			return &errs.Error{Code: errs.ERoom, Room: firstRoom(rooms), Listener: l.ID, Field: "pos", Message: fmt.Sprintf("listener must be inside exactly one room, found %d", len(rooms))}
		}
	}
	sourceIDs := map[string]bool{}
	for _, s := range level.Sources {
		if s.ID == "" {
			return errs.Decode("", "sources.id", "source id is empty")
		}
		if sourceIDs[s.ID] {
			return errs.Decode("", "sources.id", "duplicate source id %s", s.ID)
		}
		sourceIDs[s.ID] = true
		if !finite(s.Pos.X) || !finite(s.Pos.Y) {
			return errs.Decode("", "sources."+s.ID+".pos", "source position is not finite")
		}
		rooms := roomsContaining(level, s.Pos)
		if len(rooms) != 1 {
			return &errs.Error{Code: errs.ERoom, Room: firstRoom(rooms), Source: s.ID, Field: "pos", Message: fmt.Sprintf("source must be inside exactly one room, found %d", len(rooms))}
		}
	}
	return nil
}

func rectsOverlap(a, b model.Rect) bool {
	return a.X < b.Right()-geomEpsilon && a.Right() > b.X+geomEpsilon &&
		a.Y < b.Bottom()-geomEpsilon && a.Bottom() > b.Y+geomEpsilon
}

func openingInsideWall(w model.Wall, o model.Opening) bool {
	dx, dy := w.End.X-w.Start.X, w.End.Y-w.Start.Y
	l2 := dx*dx + dy*dy
	length := math.Sqrt(l2)
	if length <= geomEpsilon {
		return false
	}
	px, py := o.Center.X-w.Start.X, o.Center.Y-w.Start.Y
	t := (px*dx + py*dy) / length
	perp := math.Abs(px*dy-py*dx) / length
	if perp > geomEpsilon {
		return false
	}
	half := o.Width / 2
	return t-half >= -geomEpsilon && t+half <= length+geomEpsilon
}

func roomsContaining(level model.Level, p model.Point) []string {
	out := []string{}
	for _, r := range level.Rooms {
		if r.Rect.ContainsInclusive(p) {
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out
}

func firstRoom(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func RoomAt(level model.Level, p model.Point) (string, error) {
	rooms := roomsContaining(level, p)
	if len(rooms) == 1 {
		return rooms[0], nil
	}
	return "", &errs.Error{Code: errs.ERoom, Room: firstRoom(rooms), Field: "pos", Message: fmt.Sprintf("point must be inside exactly one room, found %d", len(rooms))}
}
