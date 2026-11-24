package diff

import (
	"fmt"

	"roomreverb/internal/model"
)

func CompareFullResults(expected, actual *model.FullResult) *model.Error {
	if expected == nil || actual == nil {
		return model.NewError(model.CodeDiff, "result", "nil result in comparison")
	}
	if err := compareStringSlices(expected.Order, actual.Order, "commit_order", ""); err != nil {
		return err
	}
	if len(expected.Paths) != len(actual.Paths) {
		return diffError("", "paths", "path count mismatch", len(expected.Paths), len(actual.Paths), nil)
	}
	for i := range expected.Paths {
		a, b := expected.Paths[i], actual.Paths[i]
		roomID := a.ListenerRoomID
		if a.Key != b.Key {
			return diffError(roomID, fmt.Sprintf("paths[%d].key", i), "path key mismatch", a.Key, b.Key, nil)
		}
		if err := compareStringSlices(a.RoomChain, b.RoomChain, "room_chain", roomID); err != nil {
			err.Details["path_key"] = a.Key
			return err
		}
		if err := compareStringSlices(a.DoorChain, b.DoorChain, "door_chain", roomID); err != nil {
			err.Details["path_key"] = a.Key
			return err
		}
		if err := comparePoints(a.Polyline, b.Polyline, roomID, a.Key); err != nil {
			return err
		}
		if a.Length != b.Length {
			return diffError(roomID, "length", "path length mismatch", a.Length, b.Length, map[string]any{"path_key": a.Key})
		}
		if len(a.CurveDB) != len(b.CurveDB) {
			return diffError(roomID, "curve_db", "curve length mismatch", len(a.CurveDB), len(b.CurveDB), map[string]any{"path_key": a.Key})
		}
		for j := range a.CurveDB {
			if a.CurveDB[j] != b.CurveDB[j] {
				return diffError(roomID, "curve_db", "curve sample mismatch", a.CurveDB[j], b.CurveDB[j], map[string]any{"path_key": a.Key, "sample_index": j})
			}
		}
		if a.SPLDB != b.SPLDB {
			return diffError(roomID, "spl_db", "SPL mismatch", a.SPLDB, b.SPLDB, map[string]any{"path_key": a.Key})
		}
	}

	expectedRooms := roomMap(expected.Rooms)
	actualRooms := roomMap(actual.Rooms)
	for _, roomID := range expected.Order {
		a := expectedRooms[roomID]
		b, ok := actualRooms[roomID]
		if !ok {
			return diffError(roomID, "room", "room missing in actual result", roomID, nil, nil)
		}
		if a.ParamSnapshot != b.ParamSnapshot {
			return diffError(roomID, "param_snapshot", "parameter snapshot mismatch", a.ParamSnapshot, b.ParamSnapshot, nil)
		}
		if !equalDoors(a.DoorSnapshot, b.DoorSnapshot) {
			return diffError(roomID, "door_snapshot", "door snapshot mismatch", len(a.DoorSnapshot), len(b.DoorSnapshot), nil)
		}
		if err := compareStringSlices(a.PathSummary, b.PathSummary, "path_summary", roomID); err != nil {
			return err
		}
		if err := compareStringSlices(a.ListenerIDs, b.ListenerIDs, "listener_ids", roomID); err != nil {
			return err
		}
		if len(a.SPLSummary) != len(b.SPLSummary) {
			return diffError(roomID, "spl_summary", "SPL summary length mismatch", len(a.SPLSummary), len(b.SPLSummary), nil)
		}
		for i := range a.SPLSummary {
			if a.SPLSummary[i] != b.SPLSummary[i] {
				return diffError(roomID, "spl_summary", "SPL summary mismatch", a.SPLSummary[i], b.SPLSummary[i], map[string]any{"index": i})
			}
		}
		if len(a.ListenerResults) != len(b.ListenerResults) {
			return diffError(roomID, "listener_results", "listener result count mismatch", len(a.ListenerResults), len(b.ListenerResults), nil)
		}
		for i := range a.ListenerResults {
			la, lb := a.ListenerResults[i], b.ListenerResults[i]
			if la.ListenerID != lb.ListenerID || la.RoomID != lb.RoomID {
				return diffError(roomID, fmt.Sprintf("listener_results[%d]", i), "listener identity mismatch", la, lb, nil)
			}
			if len(la.Paths) != len(lb.Paths) {
				return diffError(roomID, fmt.Sprintf("listener_results[%d].paths", i), "listener path count mismatch", len(la.Paths), len(lb.Paths), map[string]any{"listener_id": la.ListenerID})
			}
			for j := range la.Paths {
				pa, pb := la.Paths[j], lb.Paths[j]
				if pa.Key != pb.Key {
					return diffError(roomID, fmt.Sprintf("listener_results[%d].paths[%d].key", i, j), "path key mismatch", pa.Key, pb.Key, nil)
				}
				for k := range pa.CurveDB {
					if pa.CurveDB[k] != pb.CurveDB[k] {
						return diffError(roomID, "curve_db", "incremental/full curve mismatch", pa.CurveDB[k], pb.CurveDB[k], map[string]any{"path_key": pa.Key, "sample_index": k})
					}
				}
				if pa.SPLDB != pb.SPLDB {
					return diffError(roomID, "spl_db", "incremental/full SPL mismatch", pa.SPLDB, pb.SPLDB, map[string]any{"path_key": pa.Key})
				}
			}
		}
	}
	return nil
}

func roomMap(rooms []model.RoomResult) map[string]model.RoomResult {
	out := map[string]model.RoomResult{}
	for _, room := range rooms {
		out[room.RoomID] = room
	}
	return out
}

func compareStringSlices(a, b []string, field, roomID string) *model.Error {
	if len(a) != len(b) {
		return diffError(roomID, field, field+" length mismatch", a, b, nil)
	}
	for i := range a {
		if a[i] != b[i] {
			return diffError(roomID, field, field+" mismatch at index "+fmt.Sprint(i), a, b, map[string]any{"index": i})
		}
	}
	return nil
}

func comparePoints(a, b []model.Point, roomID, pathKey string) *model.Error {
	if len(a) != len(b) {
		return diffError(roomID, "polyline", "polyline length mismatch", len(a), len(b), map[string]any{"path_key": pathKey})
	}
	for i := range a {
		if a[i] != b[i] {
			return diffError(roomID, "polyline", "polyline point mismatch", a[i], b[i], map[string]any{"path_key": pathKey, "index": i})
		}
	}
	return nil
}

func equalDoors(a, b []model.Door) bool {
	if len(a) != len(b) {
		return false
	}
	am := map[string]model.Door{}
	for _, d := range a {
		am[d.ID] = d
	}
	for _, d := range b {
		if am[d.ID] != d {
			return false
		}
	}
	return true
}

func diffError(roomID, field, message string, expected, actual any, extra map[string]any) *model.Error {
	details := map[string]any{
		"field":    field,
		"expected": expected,
		"actual":   actual,
	}
	if roomID != "" {
		details["room_id"] = roomID
	}
	for k, v := range extra {
		details[k] = v
	}
	return &model.Error{Code: model.CodeDiff, Ref: roomID, Message: message, Details: details}
}
