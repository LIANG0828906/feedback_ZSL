package probe

import (
	"context"
	"strings"
	"time"

	"lightprobe/internal/diag"
	"lightprobe/internal/scene"
)

var allowedTransitions = map[RoomState]map[RoomState]bool{
	RoomPending:       {RoomSampling: true},
	RoomSampling:      {RoomReconstructed: true, RoomFailed: true},
	RoomReconstructed: {RoomSampling: true, RoomCommitted: true},
	RoomFailed:        {RoomSampling: true},
	RoomCommitted:     {},
}

func (e *Engine) transitionLocked(roomID string, to RoomState, reason string) error {
	from := e.roomStates[roomID]
	if from == to {
		return nil
	}
	if !allowedTransitions[from][to] {
		err := diag.New(diag.CodeState, "illegal room state transition %s -> %s", from, to)
		err.RoomID = roomID
		err.Field = "state"
		err.Details = map[string]any{"from": string(from), "to": string(to)}
		return err
	}
	e.roomStates[roomID] = to
	e.history = append(e.history, StateTransition{RoomID: roomID, From: from, To: to, Reason: reason})
	return nil
}

func (e *Engine) BuildAll(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buildAllLocked(ctx)
}

func (e *Engine) buildAllLocked(ctx context.Context) error {
	var firstErr error
	for _, room := range e.scene.Rooms {
		if e.roomStates[room.ID] == RoomCommitted {
			continue
		}
		if err := e.rebuildAllRoomLocked(ctx, room.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	e.evaluateAllPixelsLocked(nil)
	return firstErr
}

func (e *Engine) rebuildAllRoomLocked(ctx context.Context, roomID string) error {
	state := e.roomStates[roomID]
	if state == RoomCommitted {
		return versionFrozenError(roomID, "rebuild")
	}
	if state == RoomReconstructed || state == RoomFailed {
		if err := e.transitionLocked(roomID, RoomSampling, "retry/rebuild"); err != nil {
			return err
		}
	} else if state == RoomPending {
		if err := e.transitionLocked(roomID, RoomSampling, "start sampling"); err != nil {
			return err
		}
	}
	ids := e.allProbeIDsForRoomLocked(roomID)
	_, _, failed := e.runProbeIDsLocked(ctx, ids)
	if len(failed) > 0 {
		_ = e.transitionLocked(roomID, RoomFailed, "probe reconstruction failed")
		return (&diag.Error{
			Code: diag.CodeFault, RoomID: roomID, Field: "probes",
			ID: failed[0], Message: "one or more probes failed reconstruction",
			Details: map[string]any{"failed_probes": failed},
		})
	}
	return e.transitionLocked(roomID, RoomReconstructed, "all probes reconstructed")
}

func (e *Engine) RetryRoom(ctx context.Context, roomID string) (*RecomputeReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, _, ok := e.scene.RoomByID(roomID); !ok {
		return nil, diag.New(diag.CodeSchema, "unknown room %s", roomID)
	}
	state := e.roomStates[roomID]
	if state == RoomCommitted {
		return nil, versionFrozenError(roomID, "retry")
	}
	if state == RoomReconstructed {
		if err := e.transitionLocked(roomID, RoomSampling, "manual room retry"); err != nil {
			return nil, err
		}
	} else if state == RoomFailed || state == RoomPending {
		if err := e.transitionLocked(roomID, RoomSampling, "manual room retry"); err != nil {
			return nil, err
		}
	}
	ids := e.dirtyOrFailedForRoomLocked(roomID)
	if len(ids) == 0 {
		ids = e.allProbeIDsForRoomLocked(roomID)
	}
	subs, events, failed := e.runProbeIDsLocked(ctx, ids)
	report := &RecomputeReport{
		Jobs: len(ids), RecomputedProbes: ids, FailedProbes: failed,
		SubmissionIDs: submissionIDs(subs), QueueEvents: events,
	}
	if len(failed) > 0 {
		_ = e.transitionLocked(roomID, RoomFailed, "retry still failing")
		return report, (&diag.Error{Code: diag.CodeFault, RoomID: roomID, ID: failed[0], Message: "probe retry failed"})
	}
	if err := e.transitionLocked(roomID, RoomReconstructed, "retry succeeded"); err != nil {
		return report, err
	}
	e.evaluateAllPixelsLocked(nil)
	return report, nil
}

func (e *Engine) dirtyOrFailedForRoomLocked(roomID string) []string {
	ids := make([]string, 0)
	for _, id := range e.order {
		record := e.records[id]
		if record.RoomID == roomID && (record.Dirty || record.Status == ProbeFailed || record.Status == ProbePending) {
			ids = append(ids, id)
		}
	}
	return ids
}

func submissionIDs(subs []Submission) []string {
	out := make([]string, 0, len(subs))
	for _, sub := range subs {
		out = append(out, sub.ProbeID)
	}
	return out
}

func (e *Engine) evaluateAllPixelsLocked(affected map[string]bool) ([]string, int) {
	recomputed := make([]string, 0)
	hits := 0
	handled := make(map[string]bool)

	if affected != nil {
		nodeIDs := make([]string, 0, len(affected))
		for nodeID := range affected {
			nodeIDs = append(nodeIDs, nodeID)
		}
		if order, err := e.graph.TopologicalOrder(nodeIDs); err == nil {
			for _, nodeID := range order {
				pixelID, ok := strings.CutPrefix(nodeID, PixelNode(""))
				if !ok {
					continue
				}
				pixel, found := findPixel(e.scene, pixelID)
				if !found {
					continue
				}
				e.evaluatePixelLocked(pixel)
				handled[pixel.ID] = true
				recomputed = append(recomputed, pixel.ID)
			}
		}
	}

	for _, pixel := range e.scene.Pixels {
		pixelNode := PixelNode(pixel.ID)
		if handled[pixel.ID] {
			continue
		}
		if affected != nil && !affected[pixelNode] {
			if old, ok := e.pixelResults[pixel.ID]; ok {
				old.CacheHit = true
				e.pixelResults[pixel.ID] = old
				hits++
				continue
			}
		}
		e.evaluatePixelLocked(pixel)
		recomputed = append(recomputed, pixel.ID)
	}
	return recomputed, hits
}

func (e *Engine) evaluatePixelLocked(pixel scene.Pixel) {
	result := PixelResult{
		PixelID: pixel.ID, RoomID: pixel.RoomID, BoundaryID: pixel.BoundaryID,
		Position: pixel.Position, UpdatedAt: time.Now(),
	}
	interp, err := e.interpolateBlendLocked(pixel.RoomID, pixel.Position)
	if err != nil {
		result.Error = err.Error()
		if de, ok := diag.AsError(err); ok {
			result.ErrorCode = de.Code
		}
	} else {
		result.Radiance = interp.Radiance
		result.OwnerRadiance = interp.OwnerRadiance
		result.OtherRadiance = interp.OtherRadiance
		result.Alpha = interp.Alpha
		result.Corners = interp.Corners
		result.Color = scene.Shade(pixel, interp.Radiance)
	}
	result.CacheHit = false
	e.pixelResults[pixel.ID] = result
}

func findPixel(s *scene.Scene, id string) (scene.Pixel, bool) {
	for _, pixel := range s.Pixels {
		if pixel.ID == id {
			return pixel, true
		}
	}
	return scene.Pixel{}, false
}

func (e *Engine) RecomputeInvalid(ctx context.Context) (*RecomputeReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	started := time.Now()
	ids := e.dirtyProbeIDsLocked()
	roomsToRun := make(map[string]bool)
	for _, id := range ids {
		roomsToRun[e.records[id].RoomID] = true
	}
	for roomID := range roomsToRun {
		state := e.roomStates[roomID]
		if state == RoomCommitted {
			return nil, versionFrozenError(roomID, "incremental recompute")
		}
		if state == RoomReconstructed || state == RoomFailed {
			if err := e.transitionLocked(roomID, RoomSampling, "incremental invalidation"); err != nil {
				return nil, err
			}
		}
	}

	subs, events, failed := e.runProbeIDsLocked(ctx, ids)
	for roomID := range roomsToRun {
		roomFailed := false
		for _, id := range e.allProbeIDsForRoomLocked(roomID) {
			if e.records[id].Status == ProbeFailed {
				roomFailed = true
				break
			}
		}
		if roomFailed {
			_ = e.transitionLocked(roomID, RoomFailed, "invalid probe remains failed")
		} else {
			_ = e.transitionLocked(roomID, RoomReconstructed, "incremental recompute complete")
		}
	}

	affected := make(map[string]bool)
	var invalidation []InvalidationItem
	if e.lastPlan != nil {
		invalidation = append([]InvalidationItem{}, e.lastPlan.Invalidation...)
		for _, item := range e.lastPlan.Invalidation {
			affected[item.NodeID] = true
		}
	}
	pixelsRecomputed, pixelHits := e.evaluateAllPixelsLocked(affected)

	recomputedIDs := make([]string, 0, len(subs))
	for _, sub := range subs {
		if sub.Recomputed {
			recomputedIDs = append(recomputedIDs, sub.ProbeID)
		}
	}
	cacheHits := 0
	for _, id := range e.order {
		record := e.records[id]
		if record.Status == ProbeReconstructed && !containsString(ids, id) {
			cacheHits++
		}
	}
	topoNodeIDs := make([]string, 0, len(invalidation))
	for _, item := range invalidation {
		topoNodeIDs = append(topoNodeIDs, item.NodeID)
	}
	topologicalOrder, _ := e.graph.TopologicalOrder(topoNodeIDs)
	report := &RecomputeReport{
		Jobs: len(ids), RecomputedProbes: recomputedIDs, FailedProbes: failed,
		CacheHits: cacheHits, PixelCacheHits: pixelHits,
		PixelsRecomputed: pixelsRecomputed, SubmissionIDs: submissionIDs(subs),
		QueueEvents: events, Invalidation: invalidation, StartedAt: started,
		TopologicalOrder: topologicalOrder, Duration: time.Since(started),
	}
	e.lastReport = report
	if len(failed) > 0 {
		return report, (&diag.Error{Code: diag.CodeFault, ID: failed[0], Message: "recomputed probe failed"})
	}
	return report, nil
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func versionFrozenError(roomID, action string) error {
	err := diag.New(diag.CodeVer, "room %s is committed and frozen; refusing %s", roomID, action)
	err.RoomID = roomID
	err.Field = "version"
	err.Details = map[string]any{"conflict_room_id": roomID, "action": action}
	return err
}
