package engine

import (
	"encoding/json"
	"os"
	"path/filepath"

	"roomreverb/internal/levelio"
	"roomreverb/internal/model"
)

type RoomParamsEdit struct {
	RT60Seconds      *float64 `json:"rt60_seconds,omitempty"`
	EarlyDensity     *float64 `json:"early_reflection_density,omitempty"`
	ReverbGain       *float64 `json:"reverb_gain,omitempty"`
	OcclusionPerDoor *float64 `json:"occlusion_per_door_db,omitempty"`
}

func (e *Engine) UpdateRoomParams(roomID string, edit RoomParamsEdit) (*InvalidationPlan, *model.Error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	_, ok := roomByID(e.level, roomID)
	if !ok {
		return nil, model.NewError(model.CodeSchema, roomID, "unknown room")
	}
	if e.states[roomID] == StateAnalyzing || e.states[roomID] == StateSolving {
		return nil, model.NewError(model.CodeState, roomID, "cannot edit room while solve is in progress")
	}
	oldLevel := CloneLevel(e.level)
	if merr := e.openDraftLocked(roomID); merr != nil {
		return nil, merr
	}

	for i := range e.level.Rooms {
		if e.level.Rooms[i].ID != roomID {
			continue
		}
		if edit.RT60Seconds != nil {
			e.level.Rooms[i].RT60Seconds = *edit.RT60Seconds
		}
		if edit.EarlyDensity != nil {
			e.level.Rooms[i].EarlyDensity = *edit.EarlyDensity
		}
		if edit.ReverbGain != nil {
			e.level.Rooms[i].ReverbGain = *edit.ReverbGain
		}
		if edit.OcclusionPerDoor != nil {
			e.level.Rooms[i].OcclusionPerDoor = *edit.OcclusionPerDoor
		}
	}
	if merr := validateCurrentLocked(e.level); merr != nil {
		e.level = oldLevel
		return nil, merr
	}
	plan := e.applyInvalidationLocked(oldLevel)
	e.persistLevelLocked()
	return plan, nil
}

func (e *Engine) AddDoor(door model.Door) (*InvalidationPlan, *model.Error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if door.ID == "" {
		door.ID = "D-NEW"
	}
	oldLevel := CloneLevel(e.level)
	e.level.Doors = append(e.level.Doors, door)
	if merr := validateCurrentLocked(e.level); merr != nil {
		e.level = oldLevel
		return nil, merr
	}
	plan := e.applyInvalidationLocked(oldLevel)
	e.persistLevelLocked()
	return plan, nil
}

func (e *Engine) DeleteDoor(doorID string) (*InvalidationPlan, *model.Error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	oldLevel := CloneLevel(e.level)
	next := e.level.Doors[:0]
	found := false
	for _, door := range e.level.Doors {
		if door.ID == doorID {
			found = true
			continue
		}
		next = append(next, door)
	}
	if !found {
		return nil, model.NewError(model.CodeSchema, doorID, "unknown door")
	}
	e.level.Doors = next
	if merr := validateCurrentLocked(e.level); merr != nil {
		e.level = oldLevel
		return nil, merr
	}
	plan := e.applyInvalidationLocked(oldLevel)
	e.persistLevelLocked()
	return plan, nil
}

func (e *Engine) MoveWallLine(wallID string, coordinate int) (*InvalidationPlan, *model.Error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	oldLevel := CloneLevel(e.level)

	wallIndex := -1
	for i := range e.level.Walls {
		if e.level.Walls[i].ID == wallID {
			wallIndex = i
			break
		}
	}
	if wallIndex < 0 {
		return nil, model.NewError(model.CodeSchema, wallID, "unknown wall")
	}
	wall := e.level.Walls[wallIndex]
	vertical := wall.A.X == wall.B.X
	oldCoordinate := wall.A.Y
	if vertical {
		oldCoordinate = wall.A.X
	}

	e.level.Walls[wallIndex].A = movePoint(wall.A, vertical, coordinate)
	e.level.Walls[wallIndex].B = movePoint(wall.B, vertical, coordinate)
	for i := range e.level.Rooms {
		room := &e.level.Rooms[i]
		if wall.RoomAID != room.ID && wall.RoomBID != room.ID {
			continue
		}
		if vertical {
			if room.Rect.Min.X == oldCoordinate {
				room.Rect.Min.X = coordinate
			} else if room.Rect.Max.X == oldCoordinate {
				room.Rect.Max.X = coordinate
			}
		} else {
			if room.Rect.Min.Y == oldCoordinate {
				room.Rect.Min.Y = coordinate
			} else if room.Rect.Max.Y == oldCoordinate {
				room.Rect.Max.Y = coordinate
			}
		}
	}
	for i := range e.level.Doors {
		if e.level.Doors[i].WallID != wallID {
			continue
		}
		e.level.Doors[i].A = movePoint(e.level.Doors[i].A, vertical, coordinate)
		e.level.Doors[i].B = movePoint(e.level.Doors[i].B, vertical, coordinate)
	}

	if merr := validateCurrentLocked(e.level); merr != nil {
		e.level = oldLevel
		return nil, merr
	}
	plan := e.applyInvalidationLocked(oldLevel)
	e.persistLevelLocked()
	return plan, nil
}

func movePoint(p model.Point, vertical bool, coordinate int) model.Point {
	if vertical {
		p.X = coordinate
	} else {
		p.Y = coordinate
	}
	return p
}

func validateCurrentLocked(level *model.Level) *model.Error {
	if err := levelio.Validate(level); err != nil {
		if merr, ok := err.(*model.Error); ok {
			return merr
		}
		return model.NewError(model.CodeSchema, "level", err.Error())
	}
	return nil
}

func (e *Engine) applyInvalidationLocked(oldLevel *model.Level) *InvalidationPlan {
	plan := PlanInvalidation(oldLevel, e.level, e.cfg)
	e.lastPlan = plan
	e.plans = append(e.plans, *plan)

	invalidRooms := map[string]bool{}
	for _, id := range plan.InvalidRooms {
		invalidRooms[id] = true
		delete(e.roomCache, id)
		_ = e.openDraftLocked(id)
	}
	for _, listenerID := range plan.InvalidListeners {
		for key := range e.pathCache {
			if listenerFromKey(key) == listenerID {
				delete(e.pathCache, key)
			}
		}
		roomID := roomOfListener(e.level, listenerID)
		if roomID != "" && !invalidRooms[roomID] {
			plan.InvalidRooms = append(plan.InvalidRooms, roomID)
			invalidRooms[roomID] = true
			delete(e.roomCache, roomID)
			_ = e.openDraftLocked(roomID)
		}
	}
	return plan
}

func (e *Engine) persistLevelLocked() *model.Error {
	if e.cfg.LevelPath == "" {
		return nil
	}
	data, err := json.MarshalIndent(e.level, "", "  ")
	if err != nil {
		return model.NewError(model.CodeSchema, "level", err.Error())
	}
	if dir := filepath.Dir(e.cfg.LevelPath); dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := e.cfg.LevelPath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return model.NewError(model.CodeSchema, e.cfg.LevelPath, err.Error())
	}
	if err := os.Rename(tmp, e.cfg.LevelPath); err != nil {
		return model.NewError(model.CodeSchema, e.cfg.LevelPath, err.Error())
	}
	return nil
}

func (e *Engine) ScopeCheck(proposedRooms []string) *model.Error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastPlan == nil {
		plan := PlanInvalidation(e.level, e.level, e.cfg)
		e.lastPlan = plan
	}
	return CheckScope(e.lastPlan, proposedRooms)
}
