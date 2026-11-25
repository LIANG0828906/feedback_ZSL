package probe

import (
	"context"
	"encoding/json"
	"sort"

	"lightprobe/internal/checkpoint"
	"lightprobe/internal/diag"
	"lightprobe/internal/scene"
)

type RoomPayload struct {
	RoomID  string   `json:"room_id"`
	Records []Record `json:"records"`
}

type RecoveryReport struct {
	Reset          bool     `json:"reset"`
	CheckpointPath string   `json:"checkpoint_path"`
	RestoredRooms  []string `json:"restored_rooms"`
	RebuiltRooms   []string `json:"rebuilt_rooms"`
	CommittedOrder []string `json:"committed_order"`
}

func (e *Engine) EnableCheckpoint(ctx context.Context, path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	store, err := checkpoint.Open(path)
	if err != nil {
		return err
	}
	e.store = store
	e.config.CheckpointPath = path
	return nil
}

func (e *Engine) CommitRoom(ctx context.Context, roomID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitRoomLocked(ctx, roomID)
}

func (e *Engine) commitRoomLocked(ctx context.Context, roomID string) error {
	state := e.roomStates[roomID]
	if state == RoomCommitted {
		return (&diag.Error{Code: diag.CodeState, RoomID: roomID, Field: "state", Message: "room is already committed"})
	}
	if state != RoomReconstructed {
		return (&diag.Error{
			Code: diag.CodeState, RoomID: roomID, Field: "state",
			Message: "only reconstructed rooms can be committed, current state " + string(state),
			Details: map[string]any{"state": string(state)},
		})
	}

	ids := e.allProbeIDsForRoomLocked(roomID)
	payload := RoomPayload{RoomID: roomID, Records: make([]Record, 0, len(ids))}
	for _, id := range ids {
		payload.Records = append(payload.Records, *e.records[id])
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	seq := len(e.committedRoomOrder)
	snapshot := checkpoint.RoomSnapshot{
		RoomID: roomID, RoomIndex: e.roomIndexLocked(roomID), CommitSeq: seq,
		Payload: data, SHDigest: e.roomSHDigestLocked(ids),
		Dependencies: e.dependencySetForRoomLocked(roomID),
		InputHash:    e.sourceHash, SamplingVersion: scene.SamplingParameterVersion,
		AlgorithmVersion: scene.AlgorithmVersion,
	}
	if e.store != nil {
		if err := e.store.SaveRoom(ctx, snapshot); err != nil {
			return err
		}
	}
	if err := e.transitionLocked(roomID, RoomCommitted, "atomic room checkpoint committed"); err != nil {
		return err
	}
	e.committedRoomOrder = append(e.committedRoomOrder, roomID)
	e.roomCommitSeq[roomID] = seq
	return nil
}

func (e *Engine) roomIndexLocked(roomID string) int {
	_, index, ok := e.scene.RoomByID(roomID)
	if !ok {
		return -1
	}
	return index
}

func (e *Engine) roomSHDigestLocked(ids []string) string {
	digests := make([]string, 0, len(ids))
	for _, id := range ids {
		digests = append(digests, e.records[id].SHDigest)
	}
	digest, _ := checkpoint.HashJSON(digests)
	return digest
}

func (e *Engine) dependencySetForRoomLocked(roomID string) []string {
	set := make(map[string]bool)
	for id, node := range e.graph.Nodes {
		if node.RoomID == roomID {
			set[id] = true
			for dependency := range e.graph.Edges[id] {
				set[id+" <- "+dependency] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) CommitAll(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitAllLocked(ctx)
}

func (e *Engine) commitAllLocked(ctx context.Context) error {
	for _, room := range e.scene.Rooms {
		if e.roomStates[room.ID] == RoomCommitted {
			continue
		}
		if err := e.commitRoomLocked(ctx, room.ID); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) BuildAndCommitAll(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.buildAllLocked(ctx); err != nil {
		return err
	}
	return e.commitAllLocked(ctx)
}

func (e *Engine) RecoverAndResume(ctx context.Context) (*RecoveryReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	path := e.config.CheckpointPath
	if path == "" {
		return nil, diag.New(diag.CodeSchema, "checkpoint path is not configured")
	}
	store, snapshots, reset, err := checkpoint.OpenValidOrReset(
		ctx, path, e.sourceHash, scene.SamplingParameterVersion, scene.AlgorithmVersion,
	)
	if err != nil {
		return nil, err
	}
	e.store = store
	report := &RecoveryReport{Reset: reset, CheckpointPath: path}

	if !reset {
		for _, snapshot := range snapshots {
			var payload RoomPayload
			if err := json.Unmarshal(snapshot.Payload, &payload); err != nil {
				return report, err
			}
			for _, saved := range payload.Records {
				if _, ok := e.records[saved.ID]; !ok {
					return report, diag.New(diag.CodeDiff, "checkpoint contains unknown probe %s", saved.ID).WithID(saved.ID)
				}
				record := saved
				record.Status = ProbeReconstructed
				record.Dirty = false
				record.LastError = ""
				e.records[saved.ID] = &record
			}
			roomID := snapshot.RoomID
			if _, ok := e.roomStates[roomID]; !ok {
				return report, diag.New(diag.CodeDiff, "checkpoint contains unknown room %s", roomID)
			}
			if e.roomStates[roomID] != RoomCommitted {
				_ = e.transitionLocked(roomID, RoomSampling, "restore committed checkpoint")
				_ = e.transitionLocked(roomID, RoomReconstructed, "restore committed checkpoint")
				_ = e.transitionLocked(roomID, RoomCommitted, "restore committed checkpoint")
			}
			e.roomCommitSeq[roomID] = snapshot.CommitSeq
			if !containsString(e.committedRoomOrder, roomID) {
				e.committedRoomOrder = append(e.committedRoomOrder, roomID)
			}
			report.RestoredRooms = append(report.RestoredRooms, roomID)
		}
	}

	for _, room := range e.scene.Rooms {
		if e.roomStates[room.ID] == RoomCommitted {
			continue
		}
		if err := e.rebuildAllRoomLocked(ctx, room.ID); err != nil {
			return report, err
		}
		if err := e.commitRoomLocked(ctx, room.ID); err != nil {
			return report, err
		}
		report.RebuiltRooms = append(report.RebuiltRooms, room.ID)
	}
	e.evaluateAllPixelsLocked(nil)
	report.CommittedOrder = append([]string{}, e.committedRoomOrder...)
	return report, nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	store := e.store
	e.store = nil
	e.mu.Unlock()
	if store != nil {
		return store.Close()
	}
	return nil
}

func (e *Engine) Progress() Progress {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := Progress{
		CommittedRooms: len(e.committedRoomOrder), TotalRooms: len(e.scene.Rooms),
		TotalProbes: len(e.order), SubmittedProbes: len(e.submissions),
		RoomStates: make(map[string]RoomState, len(e.roomStates)),
	}
	for id, state := range e.roomStates {
		p.RoomStates[id] = state
		switch state {
		case RoomReconstructed:
			p.ReconstructedRooms++
		case RoomFailed:
			p.FailedRooms++
		}
	}
	for _, id := range e.order {
		switch e.records[id].Status {
		case ProbeReconstructed:
			p.ReconstructedProbes++
		case ProbeFailed:
			p.FailedProbes++
		}
	}
	return p
}

func (e *Engine) PixelResults() []PixelResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]PixelResult, 0, len(e.scene.Pixels))
	for _, pixel := range e.scene.Pixels {
		if result, ok := e.pixelResults[pixel.ID]; ok {
			out = append(out, result)
		}
	}
	return out
}

func (e *Engine) Submissions() []Submission {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Submission{}, e.submissions...)
}

func (e *Engine) CommittedRoomOrder() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.committedRoomOrder...)
}
