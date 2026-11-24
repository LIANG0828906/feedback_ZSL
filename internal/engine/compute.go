package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"roomreverb/internal/config"
	"roomreverb/internal/dsp"
	"roomreverb/internal/geom"
	"roomreverb/internal/model"
	"roomreverb/internal/sched"
	"roomreverb/internal/store"
	"roomreverb/internal/topology"
)

func CommitOrder(level *model.Level, cfg config.Config) []string {
	if len(cfg.CommitOrder) == 0 {
		order := make([]string, 0, len(level.Rooms))
		for _, room := range level.Rooms {
			order = append(order, room.ID)
		}
		return order
	}
	known := map[string]bool{}
	for _, room := range level.Rooms {
		known[room.ID] = true
	}
	seen := map[string]bool{}
	var order []string
	for _, id := range cfg.CommitOrder {
		if known[id] && !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}
	for _, room := range level.Rooms {
		if !seen[room.ID] {
			order = append(order, room.ID)
		}
	}
	return order
}

func ComputePaths(level *model.Level, cfg config.Config, pool *sched.Pool) ([]model.PathResult, error) {
	return computePaths(level, cfg, pool, nil)
}

func ComputeListenerPaths(level *model.Level, listenerIDs []string, cfg config.Config, pool *sched.Pool) ([]model.PathResult, error) {
	only := map[string]bool{}
	for _, id := range listenerIDs {
		only[id] = true
	}
	return computePaths(level, cfg, pool, only)
}

func computePaths(level *model.Level, cfg config.Config, pool *sched.Pool, onlyListeners map[string]bool) ([]model.PathResult, error) {
	profile, perr := cfg.Profile()
	if perr != nil {
		return nil, perr
	}
	graph := topology.Build(level, profile.Decimals)
	var tasks []sched.NamedTask
	pathResults := make([]model.PathResult, 0)
	for si := range level.Sources {
		for li := range level.Listeners {
			listener := level.Listeners[li]
			if onlyListeners != nil && !onlyListeners[listener.ID] {
				continue
			}
			source := level.Sources[si]
			key := source.ID + "->" + listener.ID
			taskIndex := len(tasks)
			pathResults = append(pathResults, model.PathResult{})
			tasks = append(tasks, sched.NamedTask{ID: key, Fn: func() error {
				listenerRoomID := geom.LocateRoom(level, listener.Pos)
				solution, terr := graph.Solve(source.ID, listener.ID)
				if terr != nil {
					if terr.Details == nil {
						terr.Details = map[string]any{}
					}
					terr.Details["listener_id"] = listener.ID
					terr.Details["listener_room_id"] = listenerRoomID
					return terr
				}
				room, ok := roomByID(level, listenerRoomID)
				if !ok {
					return model.NewError(model.CodeGeom, listener.ID, "listener room not found")
				}
				formula := dsp.FormulaInput{
					SourceLevelDB:      source.LevelDB,
					Length:             solution.Length,
					UnitsPerMeter:      level.UnitsPerMeter,
					ReverbGain:         room.ReverbGain,
					EarlyDensity:       room.EarlyDensity,
					RT60Seconds:        room.RT60Seconds,
					OcclusionPerDoorDB: room.OcclusionPerDoor,
					DoorCount:          len(solution.Doors),
					SampleDT:           level.SampleDT,
					SampleCount:        level.SampleCount,
				}
				synthesis, err := dsp.Synthesize(formula, profile, room.ID, key)
				if err != nil {
					return err
				}
				pathResults[taskIndex] = model.PathResult{
					Key:            key,
					SourceID:       source.ID,
					ListenerID:     listener.ID,
					ListenerRoomID: listenerRoomID,
					RoomChain:      append([]string(nil), solution.Rooms...),
					DoorChain:      append([]string(nil), solution.Doors...),
					Polyline:       append([]model.Point(nil), solution.Points...),
					Length:         solution.Length,
					CurveDB:        synthesis.CurveDB,
					SPLDB:          synthesis.SPLDB,
					WallClear:      solution.WallClear,
					Truncations:    synthesis.Traces,
				}
				return nil
			}})
		}
	}

	errs := runPool(pool, cfg, tasks)
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	return pathResults, nil
}

func runPool(pool *sched.Pool, cfg config.Config, tasks []sched.NamedTask) []error {
	if pool == nil {
		return sched.NewPool(1, cfg.SampleBufferLimit).Run(tasks)
	}
	return pool.Run(tasks)
}

func ComputeFull(level *model.Level, cfg config.Config, pool *sched.Pool) (*model.FullResult, error) {
	paths, err := ComputePaths(level, cfg, pool)
	if err != nil {
		return nil, err
	}
	order := CommitOrder(level, cfg)
	listenerResults := groupListeners(level, paths)
	rooms := buildRoomResults(level, cfg, order, paths, listenerResults, nil)
	return &model.FullResult{Paths: paths, Listeners: listenerResults, Rooms: rooms, Order: order}, nil
}

func groupListeners(level *model.Level, paths []model.PathResult) []model.ListenerResult {
	var out []model.ListenerResult
	for _, listener := range level.Listeners {
		roomID := geom.LocateRoom(level, listener.Pos)
		result := model.ListenerResult{ListenerID: listener.ID, RoomID: roomID}
		for _, path := range paths {
			if path.ListenerID == listener.ID {
				result.Paths = append(result.Paths, path)
			}
		}
		out = append(out, result)
	}
	return out
}

func buildRoomResults(level *model.Level, cfg config.Config, order []string, paths []model.PathResult, listeners []model.ListenerResult, revisions map[string]int) []model.RoomResult {
	roomMap := map[string]model.Room{}
	for _, room := range level.Rooms {
		roomMap[room.ID] = room
	}
	listenerByID := map[string]model.ListenerResult{}
	for _, lr := range listeners {
		listenerByID[lr.ListenerID] = lr
	}
	var results []model.RoomResult
	for i, roomID := range order {
		room := roomMap[roomID]
		var roomListeners []model.ListenerResult
		var listenerIDs []string
		var spls []float64
		var summaries []string
		for _, listener := range level.Listeners {
			if geom.LocateRoom(level, listener.Pos) != roomID {
				continue
			}
			listenerIDs = append(listenerIDs, listener.ID)
			if lr, ok := listenerByID[listener.ID]; ok {
				lrCopy := lr
				lrCopy.Paths = append([]model.PathResult(nil), lr.Paths...)
				roomListeners = append(roomListeners, lrCopy)
				for _, path := range lr.Paths {
					spls = append(spls, path.SPLDB)
					summaries = append(summaries, fmt.Sprintf("%s|rooms=%v|doors=%v|length=%.9f|spl=%.9f", path.Key, path.RoomChain, path.DoorChain, path.Length, path.SPLDB))
				}
			}
		}
		revision := 1
		if revisions != nil {
			revision = max(1, revisions[roomID])
		}
		rr := model.RoomResult{
			RoomID:          roomID,
			Revision:        revision,
			RoomHash:        hashObject(room),
			DoorHash:        hashObject(incidentDoors(level, roomID)),
			PathSummary:     summaries,
			ListenerIDs:     listenerIDs,
			SPLSummary:      spls,
			ParamSnapshot:   paramsSnapshot(room),
			DoorSnapshot:    incidentDoors(level, roomID),
			ListenerResults: roomListeners,
			ParamVersion:    cfg.ParameterVersion,
			AlgoVersion:     cfg.AlgorithmVersion,
			CommitOrder:     i,
		}
		rr.ContentHash = store.HashRoomResult(rr)
		results = append(results, rr)
	}
	return results
}

func incidentDoors(level *model.Level, roomID string) []model.Door {
	walls := map[string]model.Wall{}
	for _, wall := range level.Walls {
		walls[wall.ID] = wall
	}
	var doors []model.Door
	for _, door := range level.Doors {
		wall := walls[door.WallID]
		if wall.RoomAID == roomID || wall.RoomBID == roomID {
			doors = append(doors, door)
		}
	}
	return doors
}

func paramsSnapshot(room model.Room) model.ReverbParamsSnapshot {
	return model.ReverbParamsSnapshot{
		RT60Seconds:      room.RT60Seconds,
		EarlyDensity:     room.EarlyDensity,
		ReverbGain:       room.ReverbGain,
		OcclusionPerDoor: room.OcclusionPerDoor,
	}
}

func hashObject(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func roomByID(level *model.Level, id string) (model.Room, bool) {
	for _, room := range level.Rooms {
		if room.ID == id {
			return room, true
		}
	}
	return model.Room{}, false
}

func VerifyTruths(level *model.Level, result *model.FullResult, cfg config.Config) *model.Error {
	profile, err := cfg.Profile()
	if err != nil {
		return err
	}
	truthByKey := map[string]model.PathTruth{}
	for _, truth := range level.Truth.Paths {
		truthByKey[truth.SourceID+"->"+truth.ListenerID] = truth
	}
	for _, path := range result.Paths {
		truth, ok := truthByKey[path.Key]
		if !ok {
			return model.NewError(model.CodeSchema, path.Key, "missing truth path")
		}
		if !path.WallClear {
			return model.NewError(model.CodeGeom, path.Key, "solved polyline crosses a wall")
		}
		if perr := dsp.CheckTruth(path, truth, profile); perr != nil {
			return perr
		}
	}
	for _, listener := range level.Listeners {
		found := 0
		for _, item := range level.Truth.ListenerRooms {
			if item.ListenerID == listener.ID {
				found++
			}
		}
		if found != 1 {
			return model.NewError(model.CodeSchema, listener.ID, "listener truth occurrence count must be exactly one")
		}
	}
	return nil
}
