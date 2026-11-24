package editing

import (
	"encoding/json"
	"fmt"
	"sort"

	"levelacoustic/internal/generator"
	"levelacoustic/internal/model"
)

func CloneProject(p *model.Project) (*model.Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func ApplyOne(p *model.Project, op model.EditOp) (*model.Project, error) {
	next, err := CloneProject(p)
	if err != nil {
		return nil, err
	}
	next.Truth = nil
	switch op.Op {
	case "set_room_param":
		if op.RoomID == "" {
			return nil, model.SchemaError("ops.room_id", "set_room_param requires room_id")
		}
		if op.RT60Milliseconds == nil && op.EarlyDensityMilli == nil && op.ReverbGainMilli == nil {
			return nil, model.SchemaError("ops", "set_room_param requires at least one parameter")
		}
		room, idx, ok := model.FindRoom(next.Rooms, op.RoomID)
		if !ok {
			return nil, model.GeometryError(op.RoomID, "", "room_id", "unknown room %s", op.RoomID)
		}
		if op.RT60Milliseconds != nil {
			room.RT60Milliseconds = *op.RT60Milliseconds
		}
		if op.EarlyDensityMilli != nil {
			room.EarlyDensityMilli = *op.EarlyDensityMilli
		}
		if op.ReverbGainMilli != nil {
			room.ReverbGainMilli = *op.ReverbGainMilli
		}
		next.Rooms[idx] = room
	case "add_door":
		if op.Door == nil {
			return nil, model.SchemaError("ops.door", "add_door requires a door")
		}
		if op.Door.ID == "" || op.Door.WallID == "" {
			return nil, model.SchemaError("ops.door", "add_door requires door id and wall id")
		}
		if _, _, exists := model.FindDoor(next.Doors, op.Door.ID); exists {
			return nil, model.GeometryError("", op.Door.WallID, "door_id", "duplicate door id %s", op.Door.ID)
		}
		next.Doors = append(next.Doors, *op.Door)
		sort.Slice(next.Doors, func(i, j int) bool { return next.Doors[i].ID < next.Doors[j].ID })
	case "remove_door":
		if op.DoorID == "" {
			return nil, model.SchemaError("ops.door_id", "remove_door requires door_id")
		}
		_, idx, ok := model.FindDoor(next.Doors, op.DoorID)
		if !ok {
			return nil, model.GeometryError("", "", "door_id", "unknown door %s", op.DoorID)
		}
		next.Doors = append(next.Doors[:idx], next.Doors[idx+1:]...)
	case "drag_boundary":
		if err := dragBoundary(next, op); err != nil {
			return nil, err
		}
	default:
		return nil, model.SchemaError("ops.op", "unknown edit operation %q", op.Op)
	}
	if err := model.ValidateProject(next, model.ValidateOptions{}); err != nil {
		return nil, err
	}
	truth, err := generator.ComputeTruth(next)
	if err != nil {
		return nil, err
	}
	next.Truth = truth
	return next, nil
}

func ApplyPlan(p *model.Project, plan model.EditPlan) (*model.Project, []*model.Project, error) {
	current, err := CloneProject(p)
	if err != nil {
		return nil, nil, err
	}
	if current.Truth == nil {
		truth, err := generator.ComputeTruth(current)
		if err != nil {
			return nil, nil, err
		}
		current.Truth = truth
	}
	versions := []*model.Project{current}
	for i, op := range plan.Ops {
		next, err := ApplyOne(current, op)
		if err != nil {
			return nil, nil, fmt.Errorf("edit %d (%s): %w", i, op.Op, err)
		}
		current = next
		versions = append(versions, current)
	}
	return current, versions, nil
}

func dragBoundary(p *model.Project, op model.EditOp) error {
	if op.Axis != "x" && op.Axis != "y" {
		return model.SchemaError("ops.axis", "drag_boundary axis must be x or y")
	}
	if op.OldCoord == op.NewCoord {
		return model.GeometryError("", "", "new_coord", "drag_boundary must move the boundary")
	}
	delta := op.NewCoord - op.OldCoord
	if op.Axis == "x" {
		for i, room := range p.Rooms {
			if room.Rect.Right() == op.OldCoord {
				room.Rect.W += delta
				p.Rooms[i] = room
			} else if room.Rect.X == op.OldCoord {
				room.Rect.X += delta
				room.Rect.W -= delta
				p.Rooms[i] = room
			}
			if room.Rect.W <= 0 {
				return model.GeometryError(room.ID, "", "rect", "drag makes room %s non-positive width", room.ID)
			}
		}
		for i, wall := range p.Walls {
			if wall.Vertical() && wall.X1 == op.OldCoord {
				wall.X1 = op.NewCoord
				wall.X2 = op.NewCoord
				p.Walls[i] = wall
				continue
			}
			if wall.Horizontal() {
				left, _, _ := model.FindRoom(p.Rooms, wall.RoomA)
				right, _, _ := model.FindRoom(p.Rooms, wall.RoomB)
				bothLeft := left.Rect.X+left.Rect.W/2 < op.OldCoord && right.Rect.X+right.Rect.W/2 < op.OldCoord
				bothRight := left.Rect.X+left.Rect.W/2 > op.OldCoord && right.Rect.X+right.Rect.W/2 > op.OldCoord
				if bothLeft && wall.End() == op.OldCoord {
					if wall.X1 > wall.X2 {
						wall.X1 = op.NewCoord
					} else {
						wall.X2 = op.NewCoord
					}
					p.Walls[i] = wall
				} else if bothRight && wall.Start() == op.OldCoord {
					if wall.X1 < wall.X2 {
						wall.X1 = op.NewCoord
					} else {
						wall.X2 = op.NewCoord
					}
					p.Walls[i] = wall
				}
			}
		}
	} else {
		for i, room := range p.Rooms {
			if room.Rect.Bottom() == op.OldCoord {
				room.Rect.H += delta
				p.Rooms[i] = room
			} else if room.Rect.Y == op.OldCoord {
				room.Rect.Y += delta
				room.Rect.H -= delta
				p.Rooms[i] = room
			}
			if room.Rect.H <= 0 {
				return model.GeometryError(room.ID, "", "rect", "drag makes room %s non-positive height", room.ID)
			}
		}
		for i, wall := range p.Walls {
			if wall.Horizontal() && wall.Y1 == op.OldCoord {
				wall.Y1 = op.NewCoord
				wall.Y2 = op.NewCoord
				p.Walls[i] = wall
				continue
			}
			if wall.Vertical() {
				top, _, _ := model.FindRoom(p.Rooms, wall.RoomA)
				bottom, _, _ := model.FindRoom(p.Rooms, wall.RoomB)
				bothTop := top.Rect.Y+top.Rect.H/2 < op.OldCoord && bottom.Rect.Y+bottom.Rect.H/2 < op.OldCoord
				bothBottom := top.Rect.Y+top.Rect.H/2 > op.OldCoord && bottom.Rect.Y+bottom.Rect.H/2 > op.OldCoord
				if bothTop && wall.End() == op.OldCoord {
					if wall.Y1 > wall.Y2 {
						wall.Y1 = op.NewCoord
					} else {
						wall.Y2 = op.NewCoord
					}
					p.Walls[i] = wall
				} else if bothBottom && wall.Start() == op.OldCoord {
					if wall.Y1 < wall.Y2 {
						wall.Y1 = op.NewCoord
					} else {
						wall.Y2 = op.NewCoord
					}
					p.Walls[i] = wall
				}
			}
		}
	}
	for _, door := range p.Doors {
		wall, _, ok := model.FindWall(p.Walls, door.WallID)
		if !ok {
			return model.GeometryError("", door.WallID, "wall_id", "door %s references unknown wall", door.ID)
		}
		if door.Start < wall.Start() || door.Start+door.Width > wall.End() {
			return model.GeometryError("", wall.ID, "opening", "drag moves wall %s so door %s no longer fits", wall.ID, door.ID)
		}
	}
	return nil
}
