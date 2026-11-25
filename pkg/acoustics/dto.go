package acoustics

import (
	"encoding/json"
	"fmt"
	"os"
)

type pointDTO struct {
	X *float64 `json:"x"`
	Y *float64 `json:"y"`
}

type rectDTO struct {
	X1 *float64 `json:"x1"`
	Y1 *float64 `json:"y1"`
	X2 *float64 `json:"x2"`
	Y2 *float64 `json:"y2"`
}

type roomDTO struct {
	ID   *string  `json:"id"`
	Rect *rectDTO `json:"rect"`
}

type wallDTO struct {
	ID    *string   `json:"id"`
	RoomA *string   `json:"room_a"`
	RoomB *string   `json:"room_b"`
	A     *pointDTO `json:"a"`
	B     *pointDTO `json:"b"`
}

type doorDTO struct {
	ID     *string   `json:"id"`
	WallID *string   `json:"wall_id"`
	Center *pointDTO `json:"center"`
	Width  *float64  `json:"width_m"`
	Open   *bool     `json:"open"`
}

type reflectorDTO struct {
	ID          *string   `json:"id"`
	RoomID      *string   `json:"room_id"`
	A           *pointDTO `json:"a"`
	B           *pointDTO `json:"b"`
	Normal      *pointDTO `json:"normal"`
	Absorption  *float64  `json:"absorption"`
	Description string    `json:"description"`
}

type sourceDTO struct {
	ID       *string   `json:"id"`
	RoomID   *string   `json:"room_id"`
	Position *pointDTO `json:"position"`
	PowerDB  *float64  `json:"power_db"`
}

type listenerDTO struct {
	ID       *string   `json:"id"`
	RoomID   *string   `json:"room_id"`
	Position *pointDTO `json:"position"`
}

type paramsDTO struct {
	ParamVersion     *string  `json:"param_version"`
	AlgorithmVersion *string  `json:"algorithm_version"`
	CoordToleranceM  *float64 `json:"coord_tolerance_m"`
	LengthToleranceM *float64 `json:"length_tolerance_m"`
	SPLToleranceDB   *float64 `json:"spl_tolerance_db"`
}

type levelDTO struct {
	SchemaVersion *string         `json:"schema_version"`
	Seed          *int64          `json:"seed"`
	Units         *string         `json:"units"`
	Params        *paramsDTO      `json:"params"`
	Rooms         []*roomDTO      `json:"rooms"`
	Walls         []*wallDTO      `json:"walls"`
	Doors         []*doorDTO      `json:"doors"`
	Reflectors    []*reflectorDTO `json:"reflectors"`
	Sources       []*sourceDTO    `json:"sources"`
	Listeners     []*listenerDTO  `json:"listeners"`
}

func LoadLevelFile(path string) (*Level, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseLevel(b)
}

func ParseLevel(b []byte) (*Level, error) {
	var raw levelDTO
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, &DomainError{Code: CodeSchema, ObjectID: "document", Field: "json", Message: err.Error()}
	}
	ve := &ValidationError{}
	addSchema := func(id, field, format string, args ...any) {
		ve.Issues = append(ve.Issues, Issue{Code: CodeSchema, ObjectID: id, Field: field, Message: fmt.Sprintf(format, args...)})
	}

	if raw.SchemaVersion == nil {
		addSchema("document", "schema_version", "field is required")
	}
	if raw.Rooms == nil {
		addSchema("document", "rooms", "field is required")
	}
	if raw.Walls == nil {
		addSchema("document", "walls", "field is required")
	}
	if raw.Doors == nil {
		addSchema("document", "doors", "field is required")
	}
	if raw.Reflectors == nil {
		addSchema("document", "reflectors", "field is required")
	}
	if raw.Sources == nil {
		addSchema("document", "sources", "field is required")
	}
	if raw.Listeners == nil {
		addSchema("document", "listeners", "field is required")
	}

	lvl := &Level{}
	if raw.SchemaVersion != nil {
		lvl.SchemaVersion = *raw.SchemaVersion
	}
	if raw.Seed != nil {
		lvl.Seed = *raw.Seed
	}
	if raw.Units != nil {
		lvl.Units = *raw.Units
	} else {
		lvl.Units = "m"
	}
	lvl.Params = DefaultParams()
	if raw.Params != nil {
		p := raw.Params
		if p.ParamVersion != nil {
			lvl.Params.ParamVersion = *p.ParamVersion
		}
		if p.AlgorithmVersion != nil {
			lvl.Params.AlgorithmVersion = *p.AlgorithmVersion
		}
		if p.CoordToleranceM != nil {
			lvl.Params.CoordToleranceM = *p.CoordToleranceM
		}
		if p.LengthToleranceM != nil {
			lvl.Params.LengthToleranceM = *p.LengthToleranceM
		}
		if p.SPLToleranceDB != nil {
			lvl.Params.SPLToleranceDB = *p.SPLToleranceDB
		}
	}

	for i, r := range raw.Rooms {
		if r == nil {
			addSchema(fmt.Sprintf("rooms[%d]", i), "room", "object is required")
			continue
		}
		id := stringValue(r.ID, fmt.Sprintf("rooms[%d]", i), "id", addSchema)
		room := Room{ID: id}
		if r.Rect == nil {
			addSchema(id, "rect", "field is required")
		} else {
			room.Rect.X1 = floatValue(r.Rect.X1, id, "rect.x1", addSchema)
			room.Rect.Y1 = floatValue(r.Rect.Y1, id, "rect.y1", addSchema)
			room.Rect.X2 = floatValue(r.Rect.X2, id, "rect.x2", addSchema)
			room.Rect.Y2 = floatValue(r.Rect.Y2, id, "rect.y2", addSchema)
		}
		lvl.Rooms = append(lvl.Rooms, room)
	}
	for i, w := range raw.Walls {
		if w == nil {
			addSchema(fmt.Sprintf("walls[%d]", i), "wall", "object is required")
			continue
		}
		id := stringValue(w.ID, fmt.Sprintf("walls[%d]", i), "id", addSchema)
		wall := Wall{ID: id}
		wall.RoomA = stringValue(w.RoomA, id, "room_a", addSchema)
		wall.RoomB = stringValue(w.RoomB, id, "room_b", addSchema)
		if w.A == nil {
			addSchema(id, "a", "field is required")
		} else {
			wall.A = pointValue(w.A, id, "a", addSchema)
		}
		if w.B == nil {
			addSchema(id, "b", "field is required")
		} else {
			wall.B = pointValue(w.B, id, "b", addSchema)
		}
		lvl.Walls = append(lvl.Walls, wall)
	}
	for i, d := range raw.Doors {
		if d == nil {
			addSchema(fmt.Sprintf("doors[%d]", i), "door", "object is required")
			continue
		}
		id := stringValue(d.ID, fmt.Sprintf("doors[%d]", i), "id", addSchema)
		door := Door{ID: id, Open: d.Open}
		door.WallID = stringValue(d.WallID, id, "wall_id", addSchema)
		if d.Center == nil {
			addSchema(id, "center", "field is required")
		} else {
			door.Center = pointValue(d.Center, id, "center", addSchema)
		}
		door.Width = floatValue(d.Width, id, "width_m", addSchema)
		lvl.Doors = append(lvl.Doors, door)
	}
	for i, r := range raw.Reflectors {
		if r == nil {
			addSchema(fmt.Sprintf("reflectors[%d]", i), "reflector", "object is required")
			continue
		}
		id := stringValue(r.ID, fmt.Sprintf("reflectors[%d]", i), "id", addSchema)
		ref := Reflector{ID: id, Description: r.Description}
		ref.RoomID = stringValue(r.RoomID, id, "room_id", addSchema)
		if r.A == nil {
			addSchema(id, "a", "field is required")
		} else {
			ref.A = pointValue(r.A, id, "a", addSchema)
		}
		if r.B == nil {
			addSchema(id, "b", "field is required")
		} else {
			ref.B = pointValue(r.B, id, "b", addSchema)
		}
		if r.Normal == nil {
			addSchema(id, "normal", "field is required")
		} else {
			ref.Normal = pointValue(r.Normal, id, "normal", addSchema)
		}
		ref.Absorption = floatValue(r.Absorption, id, "absorption", addSchema)
		lvl.Reflectors = append(lvl.Reflectors, ref)
	}
	for i, s := range raw.Sources {
		if s == nil {
			addSchema(fmt.Sprintf("sources[%d]", i), "source", "object is required")
			continue
		}
		id := stringValue(s.ID, fmt.Sprintf("sources[%d]", i), "id", addSchema)
		src := Source{ID: id}
		src.RoomID = stringValue(s.RoomID, id, "room_id", addSchema)
		if s.Position == nil {
			addSchema(id, "position", "field is required")
		} else {
			src.Position = pointValue(s.Position, id, "position", addSchema)
		}
		src.PowerDB = floatValue(s.PowerDB, id, "power_db", addSchema)
		lvl.Sources = append(lvl.Sources, src)
	}
	for i, l := range raw.Listeners {
		if l == nil {
			addSchema(fmt.Sprintf("listeners[%d]", i), "listener", "object is required")
			continue
		}
		id := stringValue(l.ID, fmt.Sprintf("listeners[%d]", i), "id", addSchema)
		lis := Listener{ID: id}
		lis.RoomID = stringValue(l.RoomID, id, "room_id", addSchema)
		if l.Position == nil {
			addSchema(id, "position", "field is required")
		} else {
			lis.Position = pointValue(l.Position, id, "position", addSchema)
		}
		lvl.Listeners = append(lvl.Listeners, lis)
	}

	if err := ve.OrNil(); err != nil {
		return nil, err
	}
	return lvl, nil
}

type schemaAdder func(id, field, format string, args ...any)

func stringValue(p *string, fallbackID, field string, add schemaAdder) string {
	if p == nil {
		add(fallbackID, field, "field is required")
		return fallbackID
	}
	if *p == "" {
		add(fallbackID, field, "must not be empty")
		return fallbackID
	}
	return *p
}

func floatValue(p *float64, id, field string, add schemaAdder) float64 {
	if p == nil {
		add(id, field, "field is required")
		return 0
	}
	return *p
}

func pointValue(p *pointDTO, id, field string, add schemaAdder) Point {
	out := Point{}
	if p.X == nil {
		add(id, field+".x", "field is required")
	} else {
		out.X = *p.X
	}
	if p.Y == nil {
		add(id, field+".y", "field is required")
	} else {
		out.Y = *p.Y
	}
	return out
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func WriteJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
