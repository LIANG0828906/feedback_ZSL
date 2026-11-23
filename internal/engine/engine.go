package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"roomreverb/internal/checkpoint"
	"roomreverb/internal/errs"
	"roomreverb/internal/fsm"
	"roomreverb/internal/model"
	"roomreverb/internal/solver"
	"roomreverb/internal/validate"
)

type Config struct {
	Solvers        solver.Config `json:"solvers"`
	ParamsVersion  int           `json:"params_version"`
	RunID          string        `json:"run_id,omitempty"`
	InterruptAfter int           `json:"interrupt_after,omitempty"`
	UseCache       bool          `json:"use_cache"`
}

type RunResult struct {
	LevelID          string                       `json:"level_id"`
	LevelHash        string                       `json:"level_hash"`
	ParamsVersion    int                          `json:"params_version"`
	AlgorithmVersion string                       `json:"algorithm_version"`
	Precision        string                       `json:"precision"`
	Rooms            map[string]solver.RoomResult `json:"rooms"`
	States           []fsm.RoomState              `json:"states"`
	CommitOrder      []string                     `json:"commit_order"`
	Progress         fsm.Progress                 `json:"progress"`
	Metrics          solver.Metrics               `json:"metrics"`
	Invalidations    []InvalidationRecord         `json:"invalidations"`
	Failures         map[string]string            `json:"failures,omitempty"`
	Reset            bool                         `json:"reset,omitempty"`
	ResetReason      string                       `json:"reset_reason,omitempty"`
}

type InvalidationRecord struct {
	RoomID               string   `json:"room_id"`
	Reason               string   `json:"reason"`
	Invalidated          bool     `json:"invalidated"`
	Reused               bool     `json:"reused"`
	ReusedFrom           string   `json:"reused_from,omitempty"`
	InvalidatedListeners []string `json:"invalidated_listeners,omitempty"`
}

func RunFull(ctx context.Context, level model.Level, cfg Config) (*RunResult, error) {
	return run(ctx, level, cfg, nil, nil)
}

func RunWithCheckpoint(ctx context.Context, level model.Level, store *checkpoint.Store, cfg Config) (*RunResult, error) {
	if store == nil {
		return nil, errs.New(errs.ECheck, "checkpoint store is required")
	}
	if cfg.RunID == "" {
		cfg.RunID = "run-" + level.ID
	}
	return run(ctx, level, cfg, store, nil)
}

func run(ctx context.Context, level model.Level, cfg Config, store *checkpoint.Store, seedRooms map[string]solver.RoomResult) (*RunResult, error) {
	if err := validate.Validate(level); err != nil {
		return nil, err
	}
	ids := checkpoint.SortedRoomIDs(level)
	levelHash := checkpoint.LevelHash(level)
	meta := checkpoint.RunMeta{
		RunID: cfg.RunID, LevelHash: levelHash,
		ParamsVersion:    cfg.ParamsVersion,
		AlgorithmVersion: model.AlgorithmVersion, Precision: model.Precision,
	}
	tracker := fsm.New(ids)
	rooms := make(map[string]solver.RoomResult, len(ids))
	for id, r := range seedRooms {
		rooms[id] = r.Clone()
	}
	result := &RunResult{
		LevelID: level.ID, LevelHash: levelHash, ParamsVersion: cfg.ParamsVersion,
		AlgorithmVersion: model.AlgorithmVersion, Precision: model.Precision,
		Rooms: rooms, Failures: map[string]string{},
	}
	if store != nil {
		prep, err := store.Prepare(ctx, meta)
		if err != nil {
			return result, err
		}
		if prep.Reset {
			result.Reset = true
			result.ResetReason = prep.ResetReason
		}
		if !snapshotsArePrefix(prep.Snapshots, ids) {
			if err := store.DiscardRun(ctx, meta.RunID); err != nil {
				return result, err
			}
			prep, err = store.Prepare(ctx, meta)
			if err != nil {
				return result, err
			}
			prep.Reset = true
			prep.ResetReason = "snapshot commit order does not match stable room order"
			result.Reset = true
			result.ResetReason = prep.ResetReason
		}
		for _, snap := range prep.Snapshots {
			rooms[snap.RoomID] = snap.Result.Clone()
			if err := hydrateCommitted(tracker, snap); err != nil {
				return buildResult(level, tracker, rooms, solver.Metrics{}, result), err
			}
		}
	}

	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := rooms[id]; !ok {
			missing = append(missing, id)
		}
	}
	var batch solver.BatchResult
	if len(missing) > 0 {
		batch = solver.SolveRooms(ctx, level, missing, cfg.Solvers)
		result.Metrics = batch.Metrics
		if err := ctx.Err(); err != nil {
			_ = tracker
			return buildResult(level, tracker, rooms, batch.Metrics, result), &errs.Error{Code: errs.ECanceled, Message: err.Error(), Wrapped: err}
		}
	}

	committedTotal := len(rooms)
	var firstErr error
	for _, id := range ids {
		if _, exists := rooms[id]; exists {
			continue
		}
		if solveErr, failed := batch.Failures[id]; failed {
			if firstErr == nil {
				firstErr = solveErr
			}
			_ = tracker.Transition(id, fsm.Connectivity, "")
			tracker.Fail(id, solveErr)
			result.Failures[id] = solveErr.Error()
			break
		}
		res, ok := batch.Rooms[id]
		if !ok {
			err := batch.Failures[id]
			if err == nil {
				err = &errs.Error{Code: errs.EInput, Room: id, Message: "solver returned neither a result nor a failure"}
			}
			if firstErr == nil {
				firstErr = err
			}
			_ = tracker.Transition(id, fsm.Connectivity, "")
			tracker.Fail(id, err)
			result.Failures[id] = err.Error()
			break
		}
		if err := tracker.Transition(id, fsm.Connectivity, ""); err != nil {
			return buildResult(level, tracker, rooms, batch.Metrics, result), err
		}
		if err := ctx.Err(); err != nil {
			tracker.Fail(id, err)
			return buildResult(level, tracker, rooms, batch.Metrics, result), &errs.Error{Code: errs.ECanceled, Room: id, Message: err.Error(), Wrapped: err}
		}
		if err := tracker.Transition(id, fsm.Parameters, ""); err != nil {
			return buildResult(level, tracker, rooms, batch.Metrics, result), err
		}
		inputHash, contentHash := roomHashes(level, res)
		frozen := fsm.Frozen{
			Params: res.Params, Portals: clonePortals(res.Portals),
			Neighbors: append([]string(nil), res.Neighbors...),
			Pairs:     clonePairs(res.Pairs), Result: res.Clone(),
			ContentHash: contentHash, ParamsVersion: cfg.ParamsVersion,
			AlgorithmVersion: model.AlgorithmVersion,
		}
		_ = inputHash
		if store != nil {
			if _, err := store.SaveRoom(ctx, meta, res); err != nil {
				tracker.Fail(id, err)
				return buildResult(level, tracker, rooms, batch.Metrics, result), err
			}
			if cfg.UseCache {
				_ = store.SaveCache(ctx, res, checkpoint.RoomInputHash(level, id), cfg.ParamsVersion)
			}
		}
		if err := tracker.Commit(id, frozen); err != nil {
			return buildResult(level, tracker, rooms, batch.Metrics, result), err
		}
		rooms[id] = res.Clone()
		committedTotal++
		if cfg.InterruptAfter > 0 && committedTotal >= cfg.InterruptAfter {
			res := buildResult(level, tracker, rooms, batch.Metrics, result)
			return res, &errs.Error{Code: errs.ECanceled, Message: "simulated termination after committed checkpoint boundary"}
		}
	}
	if store != nil && firstErr == nil {
		if err := store.MarkComplete(ctx, meta); err != nil {
			return buildResult(level, tracker, rooms, batch.Metrics, result), err
		}
	}
	res := buildResult(level, tracker, rooms, batch.Metrics, result)
	return res, firstErr
}

func buildResult(level model.Level, tracker *fsm.Tracker, rooms map[string]solver.RoomResult, metrics solver.Metrics, base *RunResult) *RunResult {
	ids := checkpoint.SortedRoomIDs(level)
	out := &RunResult{
		LevelID: level.ID, LevelHash: checkpoint.LevelHash(level),
		ParamsVersion: base.ParamsVersion, AlgorithmVersion: model.AlgorithmVersion,
		Precision: model.Precision, Rooms: make(map[string]solver.RoomResult, len(rooms)),
		States: tracker.Matrix(), Progress: tracker.Progress(),
		Metrics: metrics, Invalidations: base.Invalidations,
		Failures: map[string]string{}, Reset: base.Reset, ResetReason: base.ResetReason,
	}
	for id, r := range rooms {
		out.Rooms[id] = r.Clone()
	}
	for _, id := range ids {
		if st, ok := tracker.Get(id); ok && st.State == fsm.Committed {
			out.CommitOrder = append(out.CommitOrder, id)
		}
	}
	for id, msg := range base.Failures {
		out.Failures[id] = msg
	}
	return out
}

func hydrateCommitted(tracker *fsm.Tracker, snap checkpoint.RoomSnapshot) error {
	if err := tracker.Transition(snap.RoomID, fsm.Connectivity, ""); err != nil {
		return err
	}
	if err := tracker.Transition(snap.RoomID, fsm.Parameters, ""); err != nil {
		return err
	}
	r := snap.Result
	return tracker.Commit(snap.RoomID, fsm.Frozen{
		Params: r.Params, Portals: clonePortals(r.Portals),
		Neighbors: append([]string(nil), r.Neighbors...),
		Pairs:     clonePairs(r.Pairs), Result: r.Clone(),
		ContentHash: snap.ContentHash, ParamsVersion: snap.ParamsVersion,
		AlgorithmVersion: snap.AlgorithmVersion,
	})
}

func snapshotsArePrefix(snaps []checkpoint.RoomSnapshot, sortedIDs []string) bool {
	if len(snaps) > len(sortedIDs) {
		return false
	}
	for i, s := range snaps {
		if s.RoomID != sortedIDs[i] {
			return false
		}
	}
	return true
}

func roomHashes(level model.Level, result solver.RoomResult) (inputHash, contentHash string) {
	inputHash = checkpoint.RoomInputHash(level, result.RoomID)
	payload, _ := json.Marshal(struct {
		InputHash string            `json:"input_hash"`
		Result    solver.RoomResult `json:"result"`
	}{InputHash: inputHash, Result: result})
	sum := sha256.Sum256(payload)
	return inputHash, hex.EncodeToString(sum[:])
}

func clonePortals(in []solver.PortalRef) []solver.PortalRef {
	out := make([]solver.PortalRef, len(in))
	for i, p := range in {
		out[i] = p.Clone()
	}
	return out
}

func clonePairs(in []solver.PairResult) []solver.PairResult {
	out := make([]solver.PairResult, len(in))
	for i, p := range in {
		out[i] = p.Clone()
	}
	return out
}

func SortedRoomIDs(level model.Level) []string {
	out := make([]string, 0, len(level.Rooms))
	for _, r := range level.Rooms {
		out = append(out, r.ID)
	}
	sort.Strings(out)
	return out
}

func MustCompare(a, b *RunResult) error {
	return Compare(a, b)
}

func summary(r *RunResult) string {
	if r == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s committed=%d/%d", r.LevelID, r.Progress.Committed, r.Progress.Total)
}
