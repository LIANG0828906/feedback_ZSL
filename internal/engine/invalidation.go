package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"roomreverb/internal/config"
	"roomreverb/internal/geom"
	"roomreverb/internal/model"
	"roomreverb/internal/topology"
)

type chainSignature struct {
	Key       string   `json:"key"`
	Reachable bool     `json:"reachable"`
	Rooms     []string `json:"rooms"`
	Doors     []string `json:"doors"`
	LengthQ   string   `json:"length_q"`
}

func CloneLevel(in *model.Level) *model.Level {
	data, _ := json.Marshal(in)
	var out model.Level
	_ = json.Unmarshal(data, &out)
	return &out
}

func roomTopologySignatures(level *model.Level) map[string]string {
	roomIDs := make([]string, 0, len(level.Rooms))
	adj := map[string]map[string][]string{}
	for _, room := range level.Rooms {
		roomIDs = append(roomIDs, room.ID)
		adj[room.ID] = map[string][]string{}
	}
	wallByID := map[string]model.Wall{}
	for _, wall := range level.Walls {
		wallByID[wall.ID] = wall
	}
	for _, door := range level.Doors {
		wall := wallByID[door.WallID]
		if wall.RoomAID == "" || wall.RoomBID == "" {
			continue
		}
		adj[wall.RoomAID][wall.RoomBID] = append(adj[wall.RoomAID][wall.RoomBID], door.ID)
		adj[wall.RoomBID][wall.RoomAID] = append(adj[wall.RoomBID][wall.RoomAID], door.ID)
	}
	sort.Strings(roomIDs)
	out := map[string]string{}
	for _, roomID := range roomIDs {
		dist := map[string]int{roomID: 0}
		queue := []string{roomID}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			neighbors := make([]string, 0, len(adj[cur]))
			for neighbor := range adj[cur] {
				neighbors = append(neighbors, neighbor)
			}
			sort.Strings(neighbors)
			for _, neighbor := range neighbors {
				if _, seen := dist[neighbor]; !seen {
					dist[neighbor] = dist[cur] + 1
					queue = append(queue, neighbor)
				}
			}
		}
		keys := make([]string, 0, len(dist))
		for id := range dist {
			keys = append(keys, id)
		}
		sort.Strings(keys)
		part := ""
		for _, id := range keys {
			part += fmt.Sprintf("%s:%d;", id, dist[id])
		}
		edges := make([]string, 0)
		for neighbor, doors := range adj[roomID] {
			doors = append([]string(nil), doors...)
			sort.Strings(doors)
			edges = append(edges, neighbor+"="+fmt.Sprint(doors))
		}
		sort.Strings(edges)
		out[roomID] = part + "edges:" + fmt.Sprint(edges)
	}
	return out
}

func solveChainSignatures(level *model.Level, cfg config.Config) map[string]chainSignature {
	profile, _ := cfg.Profile()
	graph := topology.Build(level, profile.Decimals)
	out := map[string]chainSignature{}
	for _, source := range level.Sources {
		for _, listener := range level.Listeners {
			key := source.ID + "->" + listener.ID
			sol, err := graph.Solve(source.ID, listener.ID)
			if err != nil {
				out[key] = chainSignature{Key: key, Reachable: false}
				continue
			}
			out[key] = chainSignature{
				Key:       key,
				Reachable: true,
				Rooms:     append([]string(nil), sol.Rooms...),
				Doors:     append([]string(nil), sol.Doors...),
				LengthQ:   sol.LengthQ.String(),
			}
		}
	}
	return out
}

func paramChangedRooms(old, new *model.Level) map[string]bool {
	oldByID := map[string]model.Room{}
	for _, room := range old.Rooms {
		oldByID[room.ID] = room
	}
	changed := map[string]bool{}
	for _, room := range new.Rooms {
		oldRoom, ok := oldByID[room.ID]
		if !ok {
			changed[room.ID] = true
			continue
		}
		if oldRoom.RT60Seconds != room.RT60Seconds ||
			oldRoom.EarlyDensity != room.EarlyDensity ||
			oldRoom.ReverbGain != room.ReverbGain ||
			oldRoom.OcclusionPerDoor != room.OcclusionPerDoor {
			changed[room.ID] = true
		}
	}
	return changed
}

func PlanInvalidation(oldLevel, newLevel *model.Level, cfg config.Config) *InvalidationPlan {
	oldTopo := roomTopologySignatures(oldLevel)
	newTopo := roomTopologySignatures(newLevel)
	oldChains := solveChainSignatures(oldLevel, cfg)
	newChains := solveChainSignatures(newLevel, cfg)
	paramsChanged := paramChangedRooms(oldLevel, newLevel)

	topoChanged := map[string]bool{}
	for id, sig := range newTopo {
		if oldTopo[id] != sig {
			topoChanged[id] = true
		}
	}

	chainChanged := map[string]bool{}
	chainRoomTouched := map[string]bool{}
	invalidListeners := map[string]bool{}
	for key, newSig := range newChains {
		oldSig := oldChains[key]
		if fmt.Sprint(oldSig) != fmt.Sprint(newSig) {
			chainChanged[key] = true
			listenerID := listenerFromKey(key)
			invalidListeners[listenerID] = true
			for _, roomID := range oldSig.Rooms {
				chainRoomTouched[roomID] = true
			}
			for _, roomID := range newSig.Rooms {
				chainRoomTouched[roomID] = true
			}
		}
	}

	listenerRoom := map[string]string{}
	for _, listener := range newLevel.Listeners {
		listenerRoom[listener.ID] = roomOfListener(newLevel, listener.ID)
		if paramsChanged[listenerRoom[listener.ID]] {
			invalidListeners[listener.ID] = true
		}
	}

	plan := &InvalidationPlan{GeneratedAt: time.Now()}
	for _, room := range newLevel.Rooms {
		var reasons []string
		if paramsChanged[room.ID] {
			reasons = append(reasons, "reverb_parameter_changed")
		}
		if topoChanged[room.ID] {
			reasons = append(reasons, "door_topology_signature_changed")
		}
		if chainRoomTouched[room.ID] && !topoChanged[room.ID] {
			reasons = append(reasons, "path_chain_or_geometry_changed")
		}
		invalid := len(reasons) > 0
		item := InvalidationItem{ID: room.ID, Kind: "room", RoomID: room.ID, Reasons: reasons, Reused: !invalid}
		if invalid {
			item.Reason = joinReasons(reasons)
			plan.InvalidRooms = append(plan.InvalidRooms, room.ID)
		} else {
			item.Reason = "unchanged_dependencies_reuse_persistent_cache"
			plan.ReusedRooms = append(plan.ReusedRooms, room.ID)
		}
		plan.Items = append(plan.Items, item)
	}

	for _, listener := range newLevel.Listeners {
		roomID := listenerRoom[listener.ID]
		var reasons []string
		if paramsChanged[roomID] {
			reasons = append(reasons, "listener_room_reverb_parameters_changed")
		}
		for key := range chainChanged {
			if listenerFromKey(key) == listener.ID {
				reasons = append(reasons, "source_listener_chain_or_length_changed")
				break
			}
		}
		invalid := len(reasons) > 0
		item := InvalidationItem{ID: listener.ID, Kind: "listener", RoomID: roomID, Reasons: reasons, Reused: !invalid}
		if invalid {
			item.Reason = joinReasons(reasons)
			plan.InvalidListeners = append(plan.InvalidListeners, listener.ID)
		} else {
			item.Reason = "unchanged_listener_results_reuse_cache"
			plan.ReusedListeners = append(plan.ReusedListeners, listener.ID)
		}
		plan.Items = append(plan.Items, item)
	}
	return plan
}

func CheckScope(plan *InvalidationPlan, proposedRooms []string) *model.Error {
	reusable := map[string]bool{}
	for _, id := range plan.ReusedRooms {
		reusable[id] = true
	}
	for _, id := range proposedRooms {
		if reusable[id] {
			return model.NewError(model.CodeScope, id, "scope expansion would falsely invalidate an unaffected room; exact dependency plan marks it reusable")
		}
	}
	return nil
}

func listenerFromKey(key string) string {
	for i := 0; i+2 < len(key); i++ {
		if key[i:i+2] == "->" {
			return key[i+2:]
		}
	}
	return key
}

func roomOfListener(level *model.Level, listenerID string) string {
	for _, listener := range level.Listeners {
		if listener.ID == listenerID {
			return geom.LocateRoom(level, listener.Pos)
		}
	}
	return ""
}

func joinReasons(reasons []string) string {
	out := ""
	for i, reason := range reasons {
		if i > 0 {
			out += ";"
		}
		out += reason
	}
	return out
}
