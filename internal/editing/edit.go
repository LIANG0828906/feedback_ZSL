package editing

import (
	"fmt"
	"math"
	"sort"

	"roomreverb/internal/errs"
	"roomreverb/internal/model"
	"roomreverb/internal/validate"
)

type Result struct {
	Level   model.Level
	Rooms   []string
	Portals []string
}

func Apply(level model.Level, edit model.Edit) (Result, error) {
	out := level.Clone()
	var touchedRooms, touchedPortals []string
	switch edit.Kind {
	case model.EditSetRoomParams:
		if edit.Params == nil {
			return Result{}, errs.Decode(edit.RoomID, "params", "set_room_params requires params")
		}
		idx := roomIndex(out, edit.RoomID)
		if idx < 0 {
			return Result{}, &errs.Error{Code: errs.ERoom, Room: edit.RoomID, Field: "room_id", Message: "unknown room"}
		}
		probe := out.Rooms[idx]
		probe.Params = *edit.Params
		if err := probe.ValidateParams(); err != nil {
			field := "params"
			switch {
			case edit.Params.T60MS < 50 || edit.Params.T60MS > 10000:
				field = "t60_ms"
			case edit.Params.EarlyDensity < 0 || edit.Params.EarlyDensity > 1:
				field = "early_density"
			case edit.Params.ReverbGain < 0 || edit.Params.ReverbGain > 4:
				field = "reverb_gain"
			}
			return Result{}, &errs.Error{Code: errs.EPara, Room: edit.RoomID, Field: field, Message: err.Error()}
		}
		out.Rooms[idx].Params = *edit.Params
		touchedRooms = append(touchedRooms, edit.RoomID)

	case model.EditAddPortal:
		if edit.Portal == nil {
			return Result{}, errs.Decode("", "portal", "add_portal requires portal")
		}
		if _, ok := out.Opening(edit.Portal.ID); ok {
			return Result{}, &errs.Error{Code: errs.ERoom, Room: firstRoom(out, edit.Portal.WallID), Field: "portal.id", Message: "duplicate portal id " + edit.Portal.ID}
		}
		p := *edit.Portal
		p.Active = true
		out.Openings = append(out.Openings, p)
		sort.Slice(out.Openings, func(i, j int) bool { return out.Openings[i].ID < out.Openings[j].ID })
		touchedPortals = append(touchedPortals, p.ID)
		if w, ok := out.Wall(p.WallID); ok {
			touchedRooms = append(touchedRooms, w.Rooms...)
		}

	case model.EditRemovePortal:
		idx := openingIndex(out, edit.PortalID)
		if idx < 0 {
			return Result{}, &errs.Error{Code: errs.ERoom, Field: "portal_id", Message: "unknown portal " + edit.PortalID}
		}
		p := out.Openings[idx]
		if w, ok := out.Wall(p.WallID); ok {
			touchedRooms = append(touchedRooms, w.Rooms...)
		}
		touchedPortals = append(touchedPortals, p.ID)
		out.Openings = append(out.Openings[:idx], out.Openings[idx+1:]...)

	case model.EditMoveRoomEdge:
		movedRooms, movedPortals, err := moveRoomEdge(&out, edit.RoomID, edit.Edge, edit.DeltaMM)
		if err != nil {
			return Result{}, err
		}
		touchedRooms = append(touchedRooms, movedRooms...)
		touchedPortals = append(touchedPortals, movedPortals...)

	default:
		return Result{}, &errs.Error{Code: errs.EInput, Field: "kind", Message: fmt.Sprintf("unknown edit kind %q", edit.Kind)}
	}
	if err := validate.Validate(out); err != nil {
		return Result{}, err
	}
	return Result{Level: out, Rooms: uniqueSorted(touchedRooms), Portals: uniqueSorted(touchedPortals)}, nil
}

func moveRoomEdge(level *model.Level, roomID, edge string, delta float64) ([]string, []string, error) {
	if math.IsNaN(delta) || math.IsInf(delta, 0) || delta == 0 {
		return nil, nil, &errs.Error{Code: errs.EInput, Room: roomID, Field: "delta_mm", Message: "delta must be a non-zero finite number"}
	}
	idx := roomIndex(*level, roomID)
	if idx < 0 {
		return nil, nil, &errs.Error{Code: errs.ERoom, Room: roomID, Field: "room_id", Message: "unknown room"}
	}
	room := level.Rooms[idx]
	vertical := edge == "left" || edge == "right"
	horizontal := edge == "top" || edge == "bottom"
	if !vertical && !horizontal {
		return nil, nil, &errs.Error{Code: errs.EInput, Room: roomID, Field: "edge", Message: "edge must be left, right, top or bottom"}
	}
	var edgeValue float64
	if vertical {
		if edge == "left" {
			edgeValue = room.Rect.X
		} else {
			edgeValue = room.Rect.Right()
		}
	} else {
		if edge == "top" {
			edgeValue = room.Rect.Y
		} else {
			edgeValue = room.Rect.Bottom()
		}
	}
	const eps = 1e-5
	var affectedWalls []int
	for i, w := range level.Walls {
		if !contains(w.Rooms, roomID) {
			continue
		}
		wallVertical := math.Abs(w.Start.X-w.End.X) <= eps
		wallHorizontal := math.Abs(w.Start.Y-w.End.Y) <= eps
		if vertical && wallVertical && math.Abs(w.Start.X-edgeValue) <= eps {
			affectedWalls = append(affectedWalls, i)
		}
		if horizontal && wallHorizontal && math.Abs(w.Start.Y-edgeValue) <= eps {
			affectedWalls = append(affectedWalls, i)
		}
	}

	// Fallback for a perimeter edge without an explicit wall segment.
	if len(affectedWalls) == 0 {
		r := &level.Rooms[idx]
		switch edge {
		case "left":
			r.Rect.X += delta
			r.Rect.W -= delta
		case "right":
			r.Rect.W += delta
		case "top":
			r.Rect.Y += delta
			r.Rect.H -= delta
		case "bottom":
			r.Rect.H += delta
		}
		if r.Rect.W <= 0 || r.Rect.H <= 0 {
			return nil, nil, &errs.Error{Code: errs.ERoom, Room: roomID, Field: "rect", Message: "edge move produced a non-positive room size"}
		}
		return []string{roomID}, nil, nil
	}

	touchedRooms := map[string]bool{}
	touchedPortals := map[string]bool{}
	for _, wi := range affectedWalls {
		w := &level.Walls[wi]
		if vertical {
			w.Start.X += delta
			w.End.X += delta
		} else {
			w.Start.Y += delta
			w.End.Y += delta
		}
		for _, rid := range w.Rooms {
			ri := roomIndex(*level, rid)
			if ri < 0 {
				continue
			}
			r := &level.Rooms[ri]
			if vertical {
				if math.Abs(r.Rect.X-edgeValue) <= eps {
					r.Rect.X += delta
					r.Rect.W -= delta
				} else if math.Abs(r.Rect.Right()-edgeValue) <= eps {
					r.Rect.W += delta
				}
			} else {
				if math.Abs(r.Rect.Y-edgeValue) <= eps {
					r.Rect.Y += delta
					r.Rect.H -= delta
				} else if math.Abs(r.Rect.Bottom()-edgeValue) <= eps {
					r.Rect.H += delta
				}
			}
			if r.Rect.W <= 0 || r.Rect.H <= 0 {
				return nil, nil, &errs.Error{Code: errs.ERoom, Room: rid, Field: "rect", Message: "edge move produced a non-positive room size"}
			}
			touchedRooms[rid] = true
		}
		for oi := range level.Openings {
			if level.Openings[oi].WallID != w.ID {
				continue
			}
			if vertical {
				level.Openings[oi].Center.X += delta
			} else {
				level.Openings[oi].Center.Y += delta
			}
			touchedPortals[level.Openings[oi].ID] = true
		}
	}
	return sortedKeys(touchedRooms), sortedKeys(touchedPortals), nil
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func roomIndex(level model.Level, id string) int {
	for i, r := range level.Rooms {
		if r.ID == id {
			return i
		}
	}
	return -1
}

func openingIndex(level model.Level, id string) int {
	for i, o := range level.Openings {
		if o.ID == id {
			return i
		}
	}
	return -1
}

func firstRoom(level model.Level, wallID string) string {
	if w, ok := level.Wall(wallID); ok && len(w.Rooms) > 0 {
		return w.Rooms[0]
	}
	return ""
}

func uniqueSorted(in []string) []string {
	m := map[string]bool{}
	for _, s := range in {
		if s != "" {
			m[s] = true
		}
	}
	return sortedKeys(m)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
