package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"levelacoustic/internal/editing"
	"levelacoustic/internal/model"
	"levelacoustic/internal/solver"
	"levelacoustic/internal/store"
)

type Phase struct {
	Name       string       `json:"name"`
	OK         bool         `json:"ok"`
	DurationMS int64        `json:"duration_ms"`
	Note       string       `json:"note,omitempty"`
	Error      *model.Error `json:"error,omitempty"`
}

type Report struct {
	OK                    bool                 `json:"ok"`
	GeneratedAt           string               `json:"generated_at"`
	Seed                  int64                `json:"seed"`
	Phases                []Phase              `json:"phases"`
	ToleranceSweep        []ToleranceCount     `json:"tolerance_sweep"`
	MonotonicTolerance    bool                 `json:"monotonic_tolerance"`
	Restore               solver.RestoreReport `json:"restore"`
	TruthDigest           string               `json:"truth_digest"`
	FullDigest            string               `json:"full_digest"`
	IncrementalDigest     string               `json:"incremental_digest"`
	ThreePathsDigestEqual bool                 `json:"three_paths_digest_equal"`
	FullProgress          string               `json:"full_progress"`
	IncrementalProgress   string               `json:"incremental_progress"`
	RestoredRooms         []string             `json:"restored_rooms"`
	StaleRooms            []string             `json:"stale_rooms"`
	FirstError            *model.Error         `json:"first_error,omitempty"`
}

type comparableResult struct {
	CommitSequence []string         `json:"commit_sequence"`
	Pairs          []comparablePair `json:"pairs"`
}

type comparablePair struct {
	SourceID         string               `json:"source_id"`
	ListenerID       string               `json:"listener_id"`
	ListenerRoomID   string               `json:"listener_room_id"`
	Reachable        bool                 `json:"reachable"`
	RoomChain        []string             `json:"room_chain"`
	DoorChain        []string             `json:"door_chain"`
	PathLengthMicro  int64                `json:"path_length_micro"`
	Segments         []model.TruthSegment `json:"segments,omitempty"`
	CurveDBMicro     []int64              `json:"curve_db_micro,omitempty"`
	SPLDBMicro       int64                `json:"spl_db_micro"`
	OcclusionDBMicro int64                `json:"occlusion_db_micro"`
}

func Run(ctx context.Context, base *model.Project, plan model.EditPlan, dbPath string, cfg solver.Config, requireCleanRestore bool) (*Report, error) {
	report := &Report{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Seed:        base.Seed,
	}
	fail := func(name string, err error) (*Report, error) {
		me, ok := model.AsError(err)
		if !ok {
			me = model.NewError(model.ErrDiff, "%v", err)
		}
		report.OK = false
		report.FirstError = me
		report.Phases = append(report.Phases, Phase{Name: name, OK: false, Error: me})
		return report, me
	}
	addPhase := func(name string, started time.Time, err error, note string) error {
		phase := Phase{Name: name, OK: err == nil, DurationMS: time.Since(started).Milliseconds(), Note: note}
		if err != nil {
			me, ok := model.AsError(err)
			if !ok {
				me = model.NewError(model.ErrDiff, "%v", err)
			}
			phase.Error = me
			report.FirstError = me
		}
		report.Phases = append(report.Phases, phase)
		return err
	}

	started := time.Now()
	finalProject, _, err := editing.ApplyPlan(base, plan)
	if err != nil {
		return fail("apply_edit_plan", err)
	}
	report.TruthDigest = digestTruth(finalProject.Truth, finalProject.Rooms)
	if err := addPhase("apply_edit_plan", started, nil, "analytic truth recomputed after every edit"); err != nil {
		return report, err
	}

	started = time.Now()
	baseFullCfg := cfg
	baseFullCfg.Store = nil
	baseFullCfg.StopAfterCommits = 0
	baseFull, err := solver.NewEngine(ctx, base, baseFullCfg)
	if err == nil {
		var baseResult *model.SolveResult
		baseResult, err = baseFull.Solve(ctx)
		if err == nil {
			err = CompareTruthTolerance(base, baseResult)
		}
	}
	if err := addPhase("base_truth_vs_full", started, err, "generator truth checked against empty-cache full solve"); err != nil {
		return report, err
	}

	started = time.Now()
	fullEngine, err := solver.NewEngine(ctx, finalProject, baseFullCfg)
	if err == nil {
		report.FullProgress = ""
		var fullResult *model.SolveResult
		fullResult, err = fullEngine.Solve(ctx)
		if err == nil {
			report.FullProgress = fullResult.Progress
			report.FullDigest = digestResult(fullResult)
			err = CompareTruthTolerance(finalProject, fullResult)
			if err == nil {
				report.ToleranceSweep = ToleranceSweep(finalProject, fullResult, []int64{0, 1, 10, 100, 1_000, 5_000, 1_000_000})
				report.MonotonicTolerance = monotonicTolerance(report.ToleranceSweep)
				if !report.MonotonicTolerance {
					err = model.NewError(model.ErrDiff, "tolerance sweep is not monotonic")
				}
			}
		}
	}
	if err := addPhase("final_truth_vs_full", started, err, "edited analytic truth checked against empty-cache full solve"); err != nil {
		return report, err
	}

	started = time.Now()
	incrementalStore, err := store.Open(dbPath)
	if err == nil {
		incCfg := cfg
		incCfg.Store = incrementalStore
		incCfg.StopAfterCommits = 0
		var incremental *solver.Engine
		incremental, err = solver.NewEngine(ctx, finalProject, incCfg)
		if err == nil {
			report.Restore = incremental.Restore()
			report.RestoredRooms = append([]string(nil), report.Restore.ReusedRooms...)
			report.StaleRooms = append([]string(nil), report.Restore.StaleRooms...)
			var fullResult *model.SolveResult
			fullResult, err = fullEngine.Solve(ctx)
			if err == nil {
				var incResult *model.SolveResult
				incResult, err = incremental.Solve(ctx)
				if err == nil {
					report.IncrementalProgress = incResult.Progress
					report.IncrementalDigest = digestResult(incResult)
					err = CompareExact(fullResult, incResult)
					report.ThreePathsDigestEqual = report.TruthDigest == report.FullDigest &&
						report.FullDigest == report.IncrementalDigest
					if err == nil && !report.ThreePathsDigestEqual {
						err = model.NewError(model.ErrDiff, "truth/full/incremental normalized digests differ")
					}
				}
			}
		}
		_ = incrementalStore.Close()
	}
	if err := addPhase("final_full_vs_incremental", started, err, "empty-cache full path compared item-by-item with SQLite incremental path"); err != nil {
		return report, err
	}

	started = time.Now()
	restoreErr := error(nil)
	if report.Restore.HadRows == 0 {
		restoreErr = model.NewError(model.ErrCheckpoint, "incremental database contains no checkpoint rows")
	}
	if requireCleanRestore {
		if report.Restore.Discarded {
			restoreErr = model.NewError(model.ErrCheckpoint, "checkpoint set was discarded: %s", report.Restore.DiscardReason)
		} else if len(report.StaleRooms) > 0 {
			restoreErr = model.NewError(model.ErrCheckpoint, "checkpoint restore still has stale rooms: %v", report.StaleRooms)
		}
	}
	if err := addPhase("checkpoint_restore", started, restoreErr, "resume checked all content hashes/versions and reused the committed room set"); err != nil {
		return report, err
	}

	report.OK = true
	return report, nil
}

func monotonicTolerance(counts []ToleranceCount) bool {
	for i := 1; i < len(counts); i++ {
		if counts[i].Failed > counts[i-1].Failed {
			return false
		}
	}
	return true
}

func digestResult(result *model.SolveResult) string {
	comparable := comparableResult{CommitSequence: append([]string(nil), result.CommitSequence...)}
	for _, pair := range result.ListenerResults {
		comparable.Pairs = append(comparable.Pairs, comparablePair{
			SourceID:         pair.SourceID,
			ListenerID:       pair.ListenerID,
			ListenerRoomID:   pair.ListenerRoomID,
			Reachable:        pair.Reachable,
			RoomChain:        append([]string(nil), pair.RoomChain...),
			DoorChain:        append([]string(nil), pair.DoorChain...),
			PathLengthMicro:  pair.PathLengthMicro,
			Segments:         append([]model.TruthSegment(nil), pair.Segments...),
			CurveDBMicro:     append([]int64(nil), pair.CurveDBMicro...),
			SPLDBMicro:       pair.SPLDBMicro,
			OcclusionDBMicro: pair.OcclusionDBMicro,
		})
	}
	sort.Slice(comparable.Pairs, func(i, j int) bool {
		return model.PairID(comparable.Pairs[i].SourceID, comparable.Pairs[i].ListenerID) <
			model.PairID(comparable.Pairs[j].SourceID, comparable.Pairs[j].ListenerID)
	})
	data, _ := json.Marshal(comparable)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func digestTruth(truth *model.Truth, rooms []model.Room) string {
	if truth == nil {
		return ""
	}
	roomSet := make(map[string]bool)
	comparable := comparableResult{}
	for _, room := range rooms {
		roomSet[room.ID] = true
	}
	for _, pair := range truth.Entries {
		roomSet[pair.ListenerRoomID] = true
		comparable.Pairs = append(comparable.Pairs, comparablePair{
			SourceID:         pair.SourceID,
			ListenerID:       pair.ListenerID,
			ListenerRoomID:   pair.ListenerRoomID,
			Reachable:        pair.Reachable,
			RoomChain:        append([]string(nil), pair.RoomChain...),
			DoorChain:        append([]string(nil), pair.DoorChain...),
			PathLengthMicro:  pair.PathLengthMicro,
			Segments:         append([]model.TruthSegment(nil), pair.Segments...),
			CurveDBMicro:     append([]int64(nil), pair.CurveDBMicro...),
			SPLDBMicro:       pair.SPLDBMicro,
			OcclusionDBMicro: pair.OcclusionDBMicro,
		})
	}
	for roomID := range roomSet {
		comparable.CommitSequence = append(comparable.CommitSequence, roomID)
	}
	sort.Strings(comparable.CommitSequence)
	sort.Slice(comparable.Pairs, func(i, j int) bool {
		return model.PairID(comparable.Pairs[i].SourceID, comparable.Pairs[i].ListenerID) <
			model.PairID(comparable.Pairs[j].SourceID, comparable.Pairs[j].ListenerID)
	})
	data, _ := json.Marshal(comparable)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (r *Report) Summary() string {
	return fmt.Sprintf("ok=%v full=%s incremental=%s three_paths_digest_equal=%v restored=%d stale=%d monotonic=%v",
		r.OK, r.FullProgress, r.IncrementalProgress, r.ThreePathsDigestEqual,
		len(r.RestoredRooms), len(r.StaleRooms), r.MonotonicTolerance)
}
