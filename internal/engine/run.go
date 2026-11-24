package engine

import (
	"errors"

	"roomreverb/internal/config"
	"roomreverb/internal/diff"
	"roomreverb/internal/model"
	"roomreverb/internal/store"
)

func (e *Engine) Recover() (int, bool, *model.Error) {
	if e.store == nil {
		return 0, false, nil
	}
	rows, err := e.store.LoadValid(e.cfg.ParameterVersion, e.cfg.AlgorithmVersion)
	if err != nil {
		if errors.Is(err, store.ErrSnapshotInvalid) {
			e.mu.Lock()
			e.snapshotInvalid = true
			e.recoveredRooms = 0
			e.mu.Unlock()
			return 0, true, nil
		}
		return 0, false, &model.Error{Code: model.CodeSchema, Message: err.Error()}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.snapshotsMatchLevelLocked(rows) {
		_ = e.store.Reset()
		e.snapshotInvalid = true
		e.recoveredRooms = 0
		e.roomCache = map[string]model.RoomResult{}
		e.pathCache = map[string]model.PathResult{}
		return 0, true, nil
	}
	for _, rr := range rows {
		e.roomCache[rr.RoomID] = rr
		e.states[rr.RoomID] = StateCommitted
		e.revisions[rr.RoomID] = max(1, rr.Revision)
		for _, listenerResult := range rr.ListenerResults {
			for _, path := range listenerResult.Paths {
				e.pathCache[path.Key] = path
			}
		}
	}
	e.recoveredRooms = len(rows)
	e.stats.RoomHits += len(rows)
	sourceCount := max(len(e.level.Sources), 1)
	e.stats.ListenerHits += len(e.pathCache) / sourceCount
	e.result = e.mergedResultLocked()
	return len(rows), false, nil
}

func (e *Engine) snapshotsMatchLevelLocked(rows []model.RoomResult) bool {
	for _, rr := range rows {
		room, ok := roomByID(e.level, rr.RoomID)
		if !ok || rr.ParamSnapshot != paramsSnapshot(room) {
			return false
		}
		expectedDoors := incidentDoors(e.level, rr.RoomID)
		if len(expectedDoors) != len(rr.DoorSnapshot) {
			return false
		}
		expected := map[string]model.Door{}
		for _, door := range expectedDoors {
			expected[door.ID] = door
		}
		for _, door := range rr.DoorSnapshot {
			if expected[door.ID] != door {
				return false
			}
		}
	}
	return true
}

func (e *Engine) RunBaseline() (*model.FullResult, *model.Error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	order := CommitOrder(e.level, e.cfg)
	for _, id := range order {
		if e.states[id] == StateCommitted {
			continue
		}
		if e.states[id] == StatePending {
			if merr := e.transitionLocked(id, StateAnalyzing); merr != nil {
				return nil, merr
			}
		}
	}

	paths, err := ComputePaths(e.level, e.cfg, e.pool)
	if err != nil {
		merr := asModelError(err)
		for _, id := range order {
			e.failRoomLocked(id, merr)
		}
		return nil, merr
	}
	if verr := VerifyTruths(e.level, &model.FullResult{Paths: paths, Order: order}, e.cfg); verr != nil {
		roomID := roomFromModelError(verr, e.level)
		if roomID != "" {
			e.failRoomLocked(roomID, verr)
		}
		for _, id := range order {
			if e.states[id] == StateAnalyzing || e.states[id] == StateSolving {
				e.failRoomLocked(id, verr)
			}
		}
		e.lastError = verr
		return nil, verr
	}

	listeners := groupListeners(e.level, paths)
	rooms := buildRoomResults(e.level, e.cfg, order, paths, listeners, e.revisions)
	for i, rr := range rooms {
		id := rr.RoomID
		if e.states[id] == StateAnalyzing {
			if merr := e.transitionLocked(id, StateSolving); merr != nil {
				return nil, merr
			}
		}
		rr.CommitOrder = i
		rr.ContentHash = store.HashRoomResult(rr)
		rr.ContentHash = store.HashRoomResult(rr)
		rooms[i] = rr
		e.roomCache[id] = rr
		for _, lr := range rr.ListenerResults {
			for _, path := range lr.Paths {
				e.pathCache[path.Key] = path
			}
		}
		if e.store != nil {
			if err := e.store.SaveRoom(rr, e.revisions[id]); err != nil {
				merr := &model.Error{Code: model.CodeSchema, Ref: id, Message: "checkpoint write failed: " + err.Error()}
				e.failRoomLocked(id, merr)
				return nil, merr
			}
		}
		if merr := e.transitionLocked(id, StateCommitted); merr != nil {
			return nil, merr
		}
		e.commitWriteOrder = append(e.commitWriteOrder, id)
	}
	e.stats.RoomMisses += len(order)
	e.stats.ListenerMisses += len(e.level.Listeners)
	e.stats.PathRecomputed += len(paths)
	e.result = &model.FullResult{Paths: paths, Listeners: listeners, Rooms: rooms, Order: order}
	return e.result, nil
}

func (e *Engine) RecomputeWithPlan(plan *InvalidationPlan) (*model.FullResult, *model.Error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recomputePlanLocked(plan)
}

func (e *Engine) recomputePlanLocked(plan *InvalidationPlan) (*model.FullResult, *model.Error) {
	if plan == nil {
		plan = e.lastPlan
	}
	if plan == nil {
		return e.mergedResultLocked(), nil
	}
	order := CommitOrder(e.level, e.cfg)
	invalidOrder := intersectOrder(order, plan.InvalidRooms)

	for _, id := range invalidOrder {
		state := e.states[id]
		switch state {
		case StateCommitted:
			if merr := e.openDraftLocked(id); merr != nil {
				return nil, merr
			}
			fallthrough
		case StatePending:
			if merr := e.transitionLocked(id, StateAnalyzing); merr != nil {
				return nil, merr
			}
		case StateFailed:
			if merr := e.transitionLocked(id, StateAnalyzing); merr != nil {
				return nil, merr
			}
		}
	}

	newPaths, err := ComputeListenerPaths(e.level, plan.InvalidListeners, e.cfg, e.pool)
	if err != nil {
		merr := asModelError(err)
		for _, id := range invalidOrder {
			e.failRoomLocked(id, merr)
		}
		return nil, merr
	}
	for _, path := range newPaths {
		e.pathCache[path.Key] = path
	}

	allPaths := e.cachedPathsLocked()
	listeners := groupListeners(e.level, allPaths)
	newRooms := buildRoomResults(e.level, e.cfg, invalidOrder, allPaths, listeners, e.revisions)
	for _, rr := range newRooms {
		id := rr.RoomID
		if e.states[id] == StateAnalyzing {
			if merr := e.transitionLocked(id, StateSolving); merr != nil {
				return nil, merr
			}
		}
	}

	for i, rr := range newRooms {
		id := rr.RoomID
		rr.CommitOrder = indexOf(order, id)
		rr.ContentHash = store.HashRoomResult(rr)
		newRooms[i] = rr
		e.roomCache[id] = rr
		if e.store != nil {
			if err := e.store.SaveRoom(rr, e.revisions[id]); err != nil {
				merr := &model.Error{Code: model.CodeSchema, Ref: id, Message: "incremental checkpoint write failed: " + err.Error()}
				e.failRoomLocked(id, merr)
				return nil, merr
			}
		}
		if merr := e.transitionLocked(id, StateCommitted); merr != nil {
			return nil, merr
		}
		e.commitWriteOrder = append(e.commitWriteOrder, id)
	}

	e.stats.RoomMisses += len(plan.InvalidRooms)
	e.stats.RoomHits += len(plan.ReusedRooms)
	e.stats.ListenerMisses += len(plan.InvalidListeners)
	e.stats.ListenerHits += len(plan.ReusedListeners)
	e.stats.PathRecomputed += len(newPaths)
	e.stats.PathReused += len(allPaths) - len(newPaths)
	e.lastPlan = plan
	e.result = e.mergedResultLocked()
	return e.result, nil
}

func (e *Engine) RetryRoom(roomID string) (*model.FullResult, *model.Error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.states[roomID]; !ok {
		return nil, model.NewError(model.CodeState, roomID, "unknown room")
	}
	if e.states[roomID] != StateFailed {
		return nil, model.NewError(model.CodeState, roomID, "retry is only legal from failed state")
	}
	if merr := e.transitionLocked(roomID, StateAnalyzing); merr != nil {
		return nil, merr
	}
	var listenerIDs []string
	for _, listener := range e.level.Listeners {
		if roomOfListener(e.level, listener.ID) == roomID {
			listenerIDs = append(listenerIDs, listener.ID)
		}
	}
	paths, err := ComputeListenerPaths(e.level, listenerIDs, e.cfg, e.pool)
	if err != nil {
		merr := asModelError(err)
		e.failFromErrorLocked(merr, []string{roomID})
		return nil, merr
	}
	for _, path := range paths {
		e.pathCache[path.Key] = path
	}
	allPaths := e.cachedPathsLocked()
	listeners := groupListeners(e.level, allPaths)
	rr := buildRoomResults(e.level, e.cfg, []string{roomID}, allPaths, listeners, e.revisions)
	if len(rr) == 1 {
		if merr := e.transitionLocked(roomID, StateSolving); merr != nil {
			return nil, merr
		}
		rr[0].CommitOrder = indexOf(CommitOrder(e.level, e.cfg), roomID)
		rr[0].ContentHash = store.HashRoomResult(rr[0])
		e.roomCache[roomID] = rr[0]
		if e.store != nil {
			if err := e.store.SaveRoom(rr[0], e.revisions[roomID]); err != nil {
				return nil, model.NewError(model.CodeSchema, roomID, err.Error())
			}
		}
		if merr := e.transitionLocked(roomID, StateCommitted); merr != nil {
			return nil, merr
		}
	}
	e.result = e.mergedResultLocked()
	return e.result, nil
}

func (e *Engine) mergedResultLocked() *model.FullResult {
	order := CommitOrder(e.level, e.cfg)
	paths := e.cachedPathsLocked()
	listeners := groupListeners(e.level, paths)
	rooms := make([]model.RoomResult, 0, len(order))
	for _, id := range order {
		if rr, ok := e.roomCache[id]; ok {
			rooms = append(rooms, rr)
		}
	}
	return &model.FullResult{Paths: paths, Listeners: listeners, Rooms: rooms, Order: order}
}

func (e *Engine) cachedPathsLocked() []model.PathResult {
	var paths []model.PathResult
	for _, source := range e.level.Sources {
		for _, listener := range e.level.Listeners {
			key := source.ID + "->" + listener.ID
			if path, ok := e.pathCache[key]; ok {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func CompareIncrementalAndFull(level *model.Level, cfg config.Config, incremental *model.FullResult) (*model.FullResult, *model.Error) {
	full, err := ComputeFull(level, cfg, nil)
	if err != nil {
		if merr, ok := err.(*model.Error); ok {
			return nil, merr
		}
		return nil, model.NewError(model.CodeDiff, "compute", err.Error())
	}
	if derr := diff.CompareFullResults(full, incremental); derr != nil {
		return full, derr
	}
	return full, nil
}

func asModelError(err error) *model.Error {
	var merr *model.Error
	if errors.As(err, &merr) {
		return merr
	}
	return model.NewError(model.CodeSchema, "compute", err.Error())
}

func (e *Engine) failFromErrorLocked(merr *model.Error, candidates []string) {
	roomID := roomFromModelError(merr, e.level)
	if roomID != "" {
		e.failRoomLocked(roomID, merr)
		return
	}
	for _, id := range candidates {
		if e.states[id] == StateAnalyzing || e.states[id] == StateSolving {
			e.failRoomLocked(id, merr)
			return
		}
	}
	e.lastError = merr
}

func roomFromModelError(merr *model.Error, level *model.Level) string {
	if merr == nil || merr.Details == nil {
		return ""
	}
	if id, ok := merr.Details["listener_room_id"].(string); ok && id != "" {
		return id
	}
	if listenerID, ok := merr.Details["listener_id"].(string); ok {
		return roomOfListener(level, listenerID)
	}
	listenerID := listenerFromKey(merr.Ref)
	if listenerID != "" && listenerID != merr.Ref {
		return roomOfListener(level, listenerID)
	}
	if len(merr.Ref) >= 2 {
		for _, listener := range level.Listeners {
			if merr.Ref == listener.ID || containsString(merr.Details, listener.ID) {
				return roomOfListener(level, listener.ID)
			}
		}
	}
	return ""
}

func containsString(details map[string]any, target string) bool {
	for _, value := range details {
		if s, ok := value.(string); ok && s == target {
			return true
		}
	}
	return false
}

func intersectOrder(order, ids []string) []string {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []string
	for _, id := range order {
		if want[id] {
			out = append(out, id)
		}
	}
	return out
}

func indexOf(order []string, id string) int {
	for i, candidate := range order {
		if candidate == id {
			return i
		}
	}
	return -1
}
