package probe

import (
	"context"

	"lightprobe/internal/diag"
)

func (e *Engine) RetryProbe(ctx context.Context, probeID string) (*RecomputeReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	record, err := e.recordByIDLocked(probeID)
	if err != nil {
		return nil, err
	}
	roomID := record.RoomID
	state := e.roomStates[roomID]
	if state == RoomCommitted {
		return nil, versionFrozenError(roomID, "retry probe "+probeID)
	}
	if state == RoomFailed || state == RoomPending || state == RoomReconstructed {
		if err := e.transitionLocked(roomID, RoomSampling, "single probe retry"); err != nil {
			return nil, err
		}
	}

	subs, events, failed := e.runProbeIDsLocked(ctx, []string{probeID})
	_, pixelHits := e.evaluateAllPixelsLocked(nil)
	report := &RecomputeReport{
		Jobs: 1, RecomputedProbes: []string{probeID}, FailedProbes: failed,
		PixelCacheHits: pixelHits, SubmissionIDs: submissionIDs(subs), QueueEvents: events,
	}
	if len(failed) > 0 {
		_ = e.transitionLocked(roomID, RoomFailed, "single probe retry failed")
		return report, (&diag.Error{Code: diag.CodeFault, ID: probeID, RoomID: roomID, Message: "probe retry failed"})
	}

	roomFailed := false
	for _, id := range e.allProbeIDsForRoomLocked(roomID) {
		if e.records[id].Status == ProbeFailed {
			roomFailed = true
			break
		}
	}
	if roomFailed {
		if err := e.transitionLocked(roomID, RoomFailed, "other probes still failed"); err != nil {
			return report, err
		}
	} else if err := e.transitionLocked(roomID, RoomReconstructed, "single probe retry succeeded"); err != nil {
		return report, err
	}
	return report, nil
}
