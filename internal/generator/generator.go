package generator

import (
	"fmt"
	"math/rand"
	"sort"

	"levelacoustic/internal/acoustics"
	"levelacoustic/internal/geom"
	"levelacoustic/internal/model"
)

type Sample struct {
	Project *model.Project `json:"project"`
	Edits   model.EditPlan `json:"edits"`
}

type Layout struct {
	Bounds model.Rect
	Rooms  []model.Room
	Walls  []model.Wall
	Doors  []model.Door
}

func Generate(seed int64) (*Sample, error) {
	rng := rand.New(rand.NewSource(seed))
	layout := buildLayout(rng)
	sourcePoints := buildSources(rng, layout.Rooms)
	listenerPoints := buildListeners(rng, layout.Rooms)
	project := &model.Project{
		SchemaVersion: model.SchemaVersion,
		Seed:          seed,
		Bounds:        layout.Bounds,
		Rooms:         layout.Rooms,
		Walls:         layout.Walls,
		Doors:         layout.Doors,
		Sources:       sourcePoints,
		Listeners:     listenerPoints,
		Tolerance:     model.DefaultTolerance(),
	}
	truth, err := ComputeTruth(project)
	if err != nil {
		return nil, err
	}
	project.Truth = truth

	d6 := model.Door{
		ID:     "D6",
		WallID: "W56",
		Start:  1950 + rng.Intn(40),
		Width:  120 + rng.Intn(20),
	}
	rt60 := int64(720)
	density := int64(850)
	gain := int64(1200)
	plan := model.EditPlan{
		Name: "deterministic-incremental-sequence",
		Ops: []model.EditOp{
			{
				Op:                "set_room_param",
				RoomID:            "R6",
				RT60Milliseconds:  &rt60,
				EarlyDensityMilli: &density,
				ReverbGainMilli:   &gain,
			},
			{Op: "add_door", Door: &d6},
			{
				Op:        "drag_boundary",
				Axis:      "x",
				OldCoord:  800,
				NewCoord:  840,
				SpanStart: 1600,
				SpanEnd:   2200,
			},
			{Op: "remove_door", DoorID: "D5"},
		},
	}
	return &Sample{Project: project, Edits: plan}, nil
}

func buildLayout(rng *rand.Rand) Layout {
	bounds := model.Rect{X: 0, Y: 0, W: 1700, H: 2400}
	rooms := []model.Room{
		{
			ID:                "R1",
			Rect:              model.Rect{X: 100, Y: 100, W: 800, H: 600},
			RT60Milliseconds:  450 + int64(rng.Intn(100)),
			EarlyDensityMilli: 280 + int64(rng.Intn(180)),
			ReverbGainMilli:   980 + int64(rng.Intn(120)),
		},
		{
			ID:                "R2",
			Rect:              model.Rect{X: 900, Y: 100, W: 700, H: 600},
			RT60Milliseconds:  580 + int64(rng.Intn(100)),
			EarlyDensityMilli: 430 + int64(rng.Intn(180)),
			ReverbGainMilli:   1080 + int64(rng.Intn(120)),
		},
		{
			ID:                "R3",
			Rect:              model.Rect{X: 100, Y: 700, W: 800, H: 600},
			RT60Milliseconds:  680 + int64(rng.Intn(100)),
			EarlyDensityMilli: 220 + int64(rng.Intn(180)),
			ReverbGainMilli:   820 + int64(rng.Intn(120)),
		},
		{
			ID:                "R4",
			Rect:              model.Rect{X: 900, Y: 700, W: 700, H: 600},
			RT60Milliseconds:  520 + int64(rng.Intn(100)),
			EarlyDensityMilli: 620 + int64(rng.Intn(180)),
			ReverbGainMilli:   1020 + int64(rng.Intn(120)),
		},
		{
			ID:                "R5",
			Rect:              model.Rect{X: 100, Y: 1600, W: 700, H: 600},
			RT60Milliseconds:  390 + int64(rng.Intn(100)),
			EarlyDensityMilli: 180 + int64(rng.Intn(180)),
			ReverbGainMilli:   900 + int64(rng.Intn(120)),
		},
		{
			ID:                "R6",
			Rect:              model.Rect{X: 800, Y: 1600, W: 700, H: 600},
			RT60Milliseconds:  610 + int64(rng.Intn(100)),
			EarlyDensityMilli: 550 + int64(rng.Intn(180)),
			ReverbGainMilli:   1120 + int64(rng.Intn(120)),
		},
	}
	walls := []model.Wall{
		{ID: "W12", X1: 900, Y1: 100, X2: 900, Y2: 700, RoomA: "R1", RoomB: "R2"},
		{ID: "W34", X1: 900, Y1: 700, X2: 900, Y2: 1300, RoomA: "R3", RoomB: "R4"},
		{ID: "W13", X1: 100, Y1: 700, X2: 900, Y2: 700, RoomA: "R1", RoomB: "R3"},
		{ID: "W24", X1: 900, Y1: 700, X2: 1600, Y2: 700, RoomA: "R2", RoomB: "R4"},
		{ID: "W56", X1: 800, Y1: 1600, X2: 800, Y2: 2200, RoomA: "R5", RoomB: "R6"},
	}
	doors := []model.Door{
		{ID: "D1", WallID: "W12", Start: 250 + rng.Intn(90), Width: 120 + rng.Intn(20)},
		{ID: "D3", WallID: "W13", Start: 360 + rng.Intn(120), Width: 120 + rng.Intn(20)},
		{ID: "D4", WallID: "W24", Start: 1080 + rng.Intn(120), Width: 120 + rng.Intn(20)},
		{ID: "D5", WallID: "W56", Start: 1700 + rng.Intn(80), Width: 120 + rng.Intn(20)},
	}
	sort.Slice(doors, func(i, j int) bool { return doors[i].ID < doors[j].ID })
	return Layout{Bounds: bounds, Rooms: rooms, Walls: walls, Doors: doors}
}

func buildSources(rng *rand.Rand, rooms []model.Room) []model.Source {
	out := make([]model.Source, 0, len(rooms))
	for i, room := range rooms {
		out = append(out, model.Source{
			ID: fmt.Sprintf("S%d", i+1),
			X:  room.Rect.X + 130 + rng.Intn(max(1, room.Rect.W-260)),
			Y:  room.Rect.Y + 130 + rng.Intn(max(1, room.Rect.H-260)),
		})
	}
	return out
}

func buildListeners(rng *rand.Rand, rooms []model.Room) []model.Listener {
	out := make([]model.Listener, 0, len(rooms)*2)
	for i, room := range rooms {
		for j := 0; j < 2; j++ {
			out = append(out, model.Listener{
				ID: fmt.Sprintf("L%d", i*2+j+1),
				X:  room.Rect.X + 120 + rng.Intn(max(1, room.Rect.W-240)),
				Y:  room.Rect.Y + 120 + rng.Intn(max(1, room.Rect.H-240)),
			})
		}
	}
	return out
}

func ComputeTruth(p *model.Project) (*model.Truth, error) {
	clone := *p
	clone.Truth = nil
	if err := model.ValidateProject(&clone, model.ValidateOptions{}); err != nil {
		return nil, err
	}
	g, err := geom.Build(&clone)
	if err != nil {
		return nil, err
	}
	roomByID := make(map[string]model.Room, len(clone.Rooms))
	for _, room := range clone.Rooms {
		roomByID[room.ID] = room
	}
	entries := make([]model.TruthEntry, 0, len(clone.Sources)*len(clone.Listeners))
	for _, source := range clone.Sources {
		for _, listener := range clone.Listeners {
			path, err := geom.EnumerateTruthPaths(g, source, listener)
			if err != nil {
				return nil, err
			}
			entry := model.TruthEntry{
				SourceID:        source.ID,
				ListenerID:      listener.ID,
				ListenerRoomID:  path.ListenerRoomID,
				Reachable:       path.Reachable,
				RoomChain:       append([]string(nil), path.RoomChain...),
				DoorChain:       append([]string(nil), path.DoorChain...),
				PathLengthMicro: path.PathLengthMicro,
				Segments:        append([]model.TruthSegment(nil), path.Segments...),
			}
			if path.Reachable {
				listenerRoom := roomByID[path.ListenerRoomID]
				chainRooms := make([]model.Room, 0, len(path.RoomChain))
				for _, roomID := range path.RoomChain {
					chainRooms = append(chainRooms, roomByID[roomID])
				}
				score := acoustics.AnalyticScore(acoustics.ChainInput{
					Rooms:           chainRooms,
					ListenerRoom:    listenerRoom,
					DoorCount:       len(path.DoorChain),
					PathLengthMicro: path.PathLengthMicro,
					CurveSamples:    model.DefaultCurveSamples,
					CurveStepMicros: model.DefaultCurveStepMicros,
				})
				entry.CurveDBMicro = score.CurveDBMicro
				entry.SPLDBMicro = score.SPLDBMicro
				entry.OcclusionDBMicro = score.OcclusionDBMicro
			}
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		a := model.PairID(entries[i].SourceID, entries[i].ListenerID)
		b := model.PairID(entries[j].SourceID, entries[j].ListenerID)
		return a < b
	})
	fingerprint, err := model.ProjectFingerprint(&clone)
	if err != nil {
		return nil, err
	}
	return &model.Truth{
		AlgorithmVersion:   model.TruthAlgorithmVersion,
		ProjectFingerprint: fingerprint,
		CurveSamples:       model.DefaultCurveSamples,
		CurveStepMicros:    model.DefaultCurveStepMicros,
		Entries:            entries,
	}, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
