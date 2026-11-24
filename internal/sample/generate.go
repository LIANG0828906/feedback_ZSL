package sample

import (
	"math"
	"math/rand"

	"roomreverb/internal/dsp"
	"roomreverb/internal/geom"
	"roomreverb/internal/model"
)

func Generate(seed int64) *model.Level {
	r := rand.New(rand.NewSource(seed))
	level := &model.Level{
		SchemaVersion: "1.0",
		Seed:          seed,
		Name:          "deterministic-six-room-reverb-workbench",
		UnitsPerMeter: 20,
		Bounds:        model.Rect{Min: model.Point{X: 0, Y: 0}, Max: model.Point{X: 1040, Y: 680}},
		SampleDT:      0.02,
		SampleCount:   48,
	}

	roomDefs := []struct {
		id string
		r  model.Rect
	}{
		{"R1", model.Rect{Min: model.Point{X: 40, Y: 40}, Max: model.Point{X: 360, Y: 340}}},
		{"R2", model.Rect{Min: model.Point{X: 360, Y: 40}, Max: model.Point{X: 680, Y: 340}}},
		{"R3", model.Rect{Min: model.Point{X: 680, Y: 40}, Max: model.Point{X: 1000, Y: 340}}},
		{"R4", model.Rect{Min: model.Point{X: 40, Y: 340}, Max: model.Point{X: 360, Y: 640}}},
		{"R5", model.Rect{Min: model.Point{X: 360, Y: 340}, Max: model.Point{X: 680, Y: 640}}},
		{"R6", model.Rect{Min: model.Point{X: 680, Y: 340}, Max: model.Point{X: 1000, Y: 640}}},
	}
	for _, def := range roomDefs {
		level.Rooms = append(level.Rooms, model.Room{
			ID:               def.id,
			Rect:             def.r,
			RT60Seconds:      0.85 + r.Float64()*0.55,
			EarlyDensity:     0.45 + r.Float64()*0.75,
			ReverbGain:       0.78 + r.Float64()*0.35,
			OcclusionPerDoor: 1.0 + r.Float64()*0.6,
		})
	}

	level.Walls = []model.Wall{
		{ID: "W12", A: model.Point{X: 360, Y: 40}, B: model.Point{X: 360, Y: 340}, RoomAID: "R1", RoomBID: "R2"},
		{ID: "W23", A: model.Point{X: 680, Y: 40}, B: model.Point{X: 680, Y: 340}, RoomAID: "R2", RoomBID: "R3"},
		{ID: "W45", A: model.Point{X: 360, Y: 340}, B: model.Point{X: 360, Y: 640}, RoomAID: "R4", RoomBID: "R5"},
		{ID: "W56", A: model.Point{X: 680, Y: 340}, B: model.Point{X: 680, Y: 640}, RoomAID: "R5", RoomBID: "R6"},
		{ID: "W14", A: model.Point{X: 40, Y: 340}, B: model.Point{X: 360, Y: 340}, RoomAID: "R1", RoomBID: "R4"},
		{ID: "W25", A: model.Point{X: 360, Y: 340}, B: model.Point{X: 680, Y: 340}, RoomAID: "R2", RoomBID: "R5"},
		{ID: "W36", A: model.Point{X: 680, Y: 340}, B: model.Point{X: 1000, Y: 340}, RoomAID: "R3", RoomBID: "R6"},

		{ID: "W1T", A: model.Point{X: 40, Y: 40}, B: model.Point{X: 360, Y: 40}, RoomAID: "R1"},
		{ID: "W2T", A: model.Point{X: 360, Y: 40}, B: model.Point{X: 680, Y: 40}, RoomAID: "R2"},
		{ID: "W3T", A: model.Point{X: 680, Y: 40}, B: model.Point{X: 1000, Y: 40}, RoomAID: "R3"},
		{ID: "W4B", A: model.Point{X: 40, Y: 640}, B: model.Point{X: 360, Y: 640}, RoomAID: "R4"},
		{ID: "W5B", A: model.Point{X: 360, Y: 640}, B: model.Point{X: 680, Y: 640}, RoomAID: "R5"},
		{ID: "W6B", A: model.Point{X: 680, Y: 640}, B: model.Point{X: 1000, Y: 640}, RoomAID: "R6"},
		{ID: "W1L", A: model.Point{X: 40, Y: 40}, B: model.Point{X: 40, Y: 340}, RoomAID: "R1"},
		{ID: "W4L", A: model.Point{X: 40, Y: 340}, B: model.Point{X: 40, Y: 640}, RoomAID: "R4"},
		{ID: "W3R", A: model.Point{X: 1000, Y: 40}, B: model.Point{X: 1000, Y: 340}, RoomAID: "R3"},
		{ID: "W6R", A: model.Point{X: 1000, Y: 340}, B: model.Point{X: 1000, Y: 640}, RoomAID: "R6"},
	}

	level.Doors = []model.Door{
		{ID: "D12", WallID: "W12", A: model.Point{X: 360, Y: 160}, B: model.Point{X: 360, Y: 200}, Kind: "door"},
		{ID: "D23", WallID: "W23", A: model.Point{X: 680, Y: 160}, B: model.Point{X: 680, Y: 200}, Kind: "door"},
		{ID: "D45", WallID: "W45", A: model.Point{X: 360, Y: 470}, B: model.Point{X: 360, Y: 510}, Kind: "door"},
		{ID: "D56", WallID: "W56", A: model.Point{X: 680, Y: 470}, B: model.Point{X: 680, Y: 510}, Kind: "door"},
		{ID: "D25", WallID: "W25", A: model.Point{X: 500, Y: 340}, B: model.Point{X: 540, Y: 340}, Kind: "opening"},
	}

	level.Sources = []model.Source{
		{ID: "S1", Pos: model.Point{X: 150, Y: 180}, LevelDB: 92},
		{ID: "S2", Pos: model.Point{X: 850, Y: 180}, LevelDB: 90},
		{ID: "S3", Pos: model.Point{X: 840, Y: 500}, LevelDB: 91},
	}
	level.Listeners = []model.Listener{
		{ID: "L1", Pos: model.Point{X: 500, Y: 500}},
		{ID: "L2", Pos: model.Point{X: 500, Y: 180}},
		{ID: "L3", Pos: model.Point{X: 180, Y: 500}},
	}

	roomByID := map[string]model.Room{}
	for _, room := range level.Rooms {
		roomByID[room.ID] = room
	}
	doorByID := map[string]model.Door{}
	for _, door := range level.Doors {
		doorByID[door.ID] = door
	}
	sourceByID := map[string]model.Source{}
	for _, source := range level.Sources {
		sourceByID[source.ID] = source
	}
	listenerByID := map[string]model.Listener{}
	for _, listener := range level.Listeners {
		listenerByID[listener.ID] = listener
		level.Truth.ListenerRooms = append(level.Truth.ListenerRooms, model.ListenerTruth{
			ListenerID: listener.ID,
			RoomID:     geom.LocateRoom(level, listener.Pos),
		})
	}

	routes := map[string]struct {
		rooms []string
		doors []string
	}{
		"S1|L1": {[]string{"R1", "R2", "R5"}, []string{"D12", "D25"}},
		"S2|L1": {[]string{"R3", "R2", "R5"}, []string{"D23", "D25"}},
		"S3|L1": {[]string{"R6", "R5"}, []string{"D56"}},
		"S1|L2": {[]string{"R1", "R2"}, []string{"D12"}},
		"S2|L2": {[]string{"R3", "R2"}, []string{"D23"}},
		"S3|L2": {[]string{"R6", "R5", "R2"}, []string{"D56", "D25"}},
		"S1|L3": {[]string{"R1", "R2", "R5", "R4"}, []string{"D12", "D25", "D45"}},
		"S2|L3": {[]string{"R3", "R2", "R5", "R4"}, []string{"D23", "D25", "D45"}},
		"S3|L3": {[]string{"R6", "R5", "R4"}, []string{"D56", "D45"}},
	}

	for _, source := range level.Sources {
		for _, listener := range level.Listeners {
			route := routes[source.ID+"|"+listener.ID]
			listenerRoomID := geom.LocateRoom(level, listener.Pos)
			room := roomByID[listenerRoomID]
			polyline := []model.Point{source.Pos}
			for _, doorID := range route.doors {
				polyline = append(polyline, geom.DoorMidpoint(doorByID[doorID]))
			}
			polyline = append(polyline, listener.Pos)
			length := 0.0
			for i := 1; i < len(polyline); i++ {
				length += math.Hypot(float64(polyline[i].X-polyline[i-1].X), float64(polyline[i].Y-polyline[i-1].Y))
			}
			formula := dsp.FormulaInput{
				SourceLevelDB:      source.LevelDB,
				Length:             length,
				UnitsPerMeter:      level.UnitsPerMeter,
				ReverbGain:         room.ReverbGain,
				EarlyDensity:       room.EarlyDensity,
				RT60Seconds:        room.RT60Seconds,
				OcclusionPerDoorDB: room.OcclusionPerDoor,
				DoorCount:          len(route.doors),
				SampleDT:           level.SampleDT,
				SampleCount:        level.SampleCount,
			}
			level.Truth.Paths = append(level.Truth.Paths, model.PathTruth{
				SourceID:   source.ID,
				ListenerID: listener.ID,
				RoomID:     listenerRoomID,
				RoomChain:  append([]string(nil), route.rooms...),
				DoorChain:  append([]string(nil), route.doors...),
				Polyline:   polyline,
				Length:     length,
				CurveDB:    dsp.AnalyticCurve(formula),
				SPLDB:      dsp.AnalyticSPL(formula),
			})
		}
	}
	return level
}
