package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"combatjudge/internal/model"
	"combatjudge/internal/store"
)

func Run(ctx context.Context, projectIn *model.Project, opts Options) (*Report, error) {
	if opts.Injection.UnitContradiction {
		return nil, model.NewError(model.CodeUnit, "$.unit_scale", projectIn.SchemaVersion, "injected fixed-scale contradiction: declared scale does not match version field encoding")
	}
	project := cloneProject(projectIn)
	if opts.Injection.UseBadStep {
		project.StepMS = opts.Injection.BadStepMS
		for i := range project.Sweeps {
			project.Sweeps[i].StepMS = opts.Injection.BadStepMS
		}
	}
	if project.StepMS <= 0 {
		return nil, model.NewError(model.CodeStep, "$.frameStepMs", project.SchemaVersion, "time step must be positive, got %d", project.StepMS)
	}
	if opts.IntervalFrames <= 0 {
		opts.IntervalFrames = DefaultIntervalFrames
	}
	if opts.SnapshotID == "" {
		opts.SnapshotID = "battle-snapshot"
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = ":memory:"
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	identity := ProjectIdentity(project)
	snap, err := st.BeginSnapshot(opts.SnapshotID, identity, model.AlgoVersion, TimebaseVersion(project), ParamsVersion(project))
	if err != nil {
		return nil, err
	}

	specs := BuildIntervals(project, opts.IntervalFrames)
	graph := BuildDependencies(project, specs)
	keys := BuildKeys(project, specs, graph)
	pool := NewPool(opts.MaxParallel, opts.MaxQueuedIntervals, opts.GeometryBufferBytes)
	defer pool.Close()

	report := &Report{
		Path: opts.PathLabel, SnapshotID: opts.SnapshotID, Generation: snap.Generation,
		Voided: snap.Voided, VoidReason: snap.VoidReason,
		Intervals: []*IntervalResult{}, Frames: []FrameResult{},
		CommitSequence: []int{}, WriteOrder: []int{}, ReusedIntervals: []int{},
		Recomputed: []int{}, Invalidations: []Invalidation{}, Failures: []FailedInterval{},
	}

	preloaded := map[int]*IntervalResult{}
	futures := map[int]*Future{}
	missNos := []int{}

	for _, spec := range specs {
		key := keys[spec.No]
		existing, err := loadExisting(st, opts.SnapshotID, snap.Generation, spec.No, key)
		if err != nil {
			return report, err
		}
		if existing != nil {
			preloaded[spec.No] = existing
			continue
		}
		cached, err := loadCachedValid(st, key)
		if err != nil {
			return report, err
		}
		if cached != nil {
			preloaded[spec.No] = cached
			continue
		}

		no := spec.No
		missNos = append(missNos, no)
		futures[no] = pool.SubmitGeometry(GeometryJob{IntervalNo: no, Run: func(buf *GeomBuffer) (*GeometryResult, error) {
			if opts.Injection.FailOnceIntervals != nil && opts.Injection.FailOnceIntervals[no] {
				delete(opts.Injection.FailOnceIntervals, no)
				return nil, &model.Error{Code: model.CodeDecode, FieldPath: fmt.Sprintf("interval[%d].injected", no), IntervalIndex: no, Message: "injected one-shot interval failure"}
			}
			return SolveIntervalGeometry(project, spec, opts.Injection, buf)
		}})
	}

	state := NewInitialState(project)
	stop := false
	for _, spec := range specs {
		if stop {
			break
		}
		key := keys[spec.No]
		deps := graph[spec.No]
		_, contentHash := MakeCacheKey(project, spec, deps, keyInputUpstream(keys, spec.No, specs))

		var ir *IntervalResult
		if pre := preloaded[spec.No]; pre != nil {
			ir = pre
			ir.CacheKey = key
			ir.Dependencies = deps
			ir.AlgorithmVersion = model.AlgoVersion
			report.ReusedIntervals = append(report.ReusedIntervals, spec.No)
			if _, currentGen, _ := existingCheckpoint(st, opts.SnapshotID, snap.Generation, spec.No); !currentGen {
				if err := commitResult(st, opts.SnapshotID, snap.Generation, ir, TimebaseVersion(project), ParamsVersion(project)); err != nil {
					return report, err
				}
			}
		} else {
			future := futures[spec.No]
			geo, geoErr := future.Get()
			if geoErr != nil {
				coded := asModelError(geoErr, spec.No)
				report.Failures = append(report.Failures, FailedInterval{IntervalNo: spec.No, Error: coded, Attempts: 1})
				drainFutures(futures, missNos, spec.No)
				report.QueueWaits = pool.QueueWaits()
				return report, coded
			}
			settleFuture := pool.SubmitSettlement(spec.No, func(_ *GeomBuffer) (*IntervalResult, error) {
				return SettleInterval(project, spec, geo, state, key, keyInputUpstream(keys, spec.No, specs), contentHash, deps)
			})
			ir, err = settleFuture.GetInterval()
			if err != nil {
				coded := asModelError(err, spec.No)
				report.Failures = append(report.Failures, FailedInterval{IntervalNo: spec.No, Error: coded, Attempts: 1})
				drainFutures(futures, missNos, spec.No)
				report.QueueWaits = pool.QueueWaits()
				return report, coded
			}
			if err := commitResult(st, opts.SnapshotID, snap.Generation, ir, TimebaseVersion(project), ParamsVersion(project)); err != nil {
				return report, err
			}
			report.Recomputed = append(report.Recomputed, spec.No)
		}

		state = ir.StateOut
		report.Intervals = append(report.Intervals, ir)
		if opts.StopAfterInterval > 0 && spec.No >= opts.StopAfterInterval {
			stop = true
		}
	}

	report.QueueWaits = pool.QueueWaits()
	committed, err := st.CommittedIntervalNumbers(opts.SnapshotID, snap.Generation)
	if err != nil {
		return report, err
	}
	writeOrder, err := st.WriteOrder(opts.SnapshotID, snap.Generation)
	if err != nil {
		return report, err
	}
	report.CommitSequence = committed
	report.WriteOrder = writeOrder
	for _, ir := range report.Intervals {
		report.Frames = append(report.Frames, ir.Frames...)
	}
	sortFrames(report.Frames)
	return report, nil
}

func keyInputUpstream(keys map[int]string, no int, specs []IntervalSpec) string {
	if no <= 1 {
		return GenesisKey
	}
	return keys[no-1]
}

func drainFutures(futures map[int]*Future, nos []int, failedNo int) {
	for _, no := range nos {
		if no <= failedNo {
			continue
		}
		if f, ok := futures[no]; ok {
			_, _ = f.Get()
		}
	}
}

func asModelError(err error, intervalNo int) *model.Error {
	var me *model.Error
	if errors.As(err, &me) {
		if me.IntervalIndex == 0 {
			me.IntervalIndex = intervalNo
		}
		return me
	}
	return &model.Error{Code: model.CodeDecode, IntervalIndex: intervalNo, Message: err.Error()}
}

func cloneProject(in *model.Project) *model.Project {
	b, _ := json.Marshal(in)
	var out model.Project
	_ = json.Unmarshal(b, &out)
	return &out
}

func loadExisting(st *store.Store, snapshotID string, generation, no int, key string) (*IntervalResult, error) {
	rec, err := st.LoadInterval(snapshotID, generation, no)
	if err != nil || rec == nil {
		return nil, err
	}
	if rec.CacheKey != key || rec.AlgoVersion != model.AlgoVersion {
		return nil, nil
	}
	var ir IntervalResult
	if err := json.Unmarshal(rec.Payload, &ir); err != nil {
		return nil, &model.Error{Code: model.CodeDecode, FieldPath: fmt.Sprintf("interval_checkpoints[%d].payload_json", no), IntervalIndex: no, Message: "persisted interval payload cannot be decoded: " + err.Error()}
	}
	if ir.ContentHash != rec.ContentHash {
		return nil, nil
	}
	return &ir, nil
}

func existingCheckpoint(st *store.Store, snapshotID string, generation, no int) (*store.IntervalRecord, bool, error) {
	rec, err := st.LoadInterval(snapshotID, generation, no)
	return rec, rec != nil, err
}

func loadCachedValid(st *store.Store, key string) (*IntervalResult, error) {
	rec, err := st.LoadCached(key)
	if err != nil || rec == nil {
		return nil, err
	}
	if rec.AlgoVersion != model.AlgoVersion {
		_ = st.DeleteCached(key)
		return nil, nil
	}
	var ir IntervalResult
	if err := json.Unmarshal(rec.Payload, &ir); err != nil {
		_ = st.DeleteCached(key)
		return nil, nil
	}
	if ir.ContentHash != rec.ContentHash {
		_ = st.DeleteCached(key)
		return nil, nil
	}
	return &ir, nil
}

func commitResult(st *store.Store, snapshotID string, generation int, ir *IntervalResult, timebaseVersion, paramsVersion string) error {
	payload, err := json.Marshal(ir)
	if err != nil {
		return err
	}
	return st.CommitInterval(snapshotID, generation, store.IntervalRecord{
		IntervalNo: ir.No, CacheKey: ir.CacheKey, StartFrame: ir.StartFrame, EndFrame: ir.EndFrame,
		Payload: payload, ContentHash: ir.ContentHash, TimebaseVersion: timebaseVersion,
		ParamsVersion: paramsVersion, AlgoVersion: model.AlgoVersion, Status: ir.Status, Attempts: 1,
	})
}

func sortFrames(frames []FrameResult) {
	sort.SliceStable(frames, func(i, j int) bool {
		if frames[i].Frame != frames[j].Frame {
			return frames[i].Frame < frames[j].Frame
		}
		return frames[i].SweepID < frames[j].SweepID
	})
}

func CompareReports(want, got *Report) *CompareMismatch {
	if len(want.Frames) != len(got.Frames) {
		frame := 0
		if len(want.Frames) > 0 {
			frame = want.Frames[0].Frame
		}
		return &CompareMismatch{Frame: frame, Field: "frames.length", Want: fmt.Sprint(len(want.Frames)), Got: fmt.Sprint(len(got.Frames))}
	}
	for i := range want.Frames {
		w := want.Frames[i]
		g := got.Frames[i]
		if w.Frame != g.Frame || w.SweepID != g.SweepID {
			return &CompareMismatch{Frame: firstFrame(w.Frame, g.Frame), SweepID: w.SweepID, Field: "frame_key", Want: fmt.Sprintf("%d/%s", w.Frame, w.SweepID), Got: fmt.Sprintf("%d/%s", g.Frame, g.SweepID)}
		}
		if w.FirstColliderNo != g.FirstColliderNo {
			return mismatch(w, "first_collider_no", w.FirstColliderNo, g.FirstColliderNo)
		}
		if w.FirstPart != g.FirstPart {
			return mismatch(w, "first_part", w.FirstPart, g.FirstPart)
		}
		if w.FirstSurface != g.FirstSurface {
			return mismatch(w, "first_surface", w.FirstSurface, g.FirstSurface)
		}
		if w.RemainingDamageQ != g.RemainingDamageQ {
			return mismatch(w, "remaining_damage_q", w.RemainingDamageQ, g.RemainingDamageQ)
		}
		if w.NearestDistanceQ != g.NearestDistanceQ {
			return mismatch(w, "nearest_distance_q", w.NearestDistanceQ, g.NearestDistanceQ)
		}
		wb, _ := json.Marshal(w.Segments)
		gb, _ := json.Marshal(g.Segments)
		if string(wb) != string(gb) {
			return &CompareMismatch{Frame: w.Frame, SweepID: w.SweepID, Field: "segments", Want: string(wb), Got: string(gb)}
		}
	}
	if fmt.Sprint(want.CommitSequence) != fmt.Sprint(got.CommitSequence) {
		return &CompareMismatch{Field: "commit_sequence", Want: fmt.Sprint(want.CommitSequence), Got: fmt.Sprint(got.CommitSequence)}
	}
	if fmt.Sprint(want.WriteOrder) != fmt.Sprint(got.WriteOrder) {
		return &CompareMismatch{Field: "write_order", Want: fmt.Sprint(want.WriteOrder), Got: fmt.Sprint(got.WriteOrder)}
	}
	return nil
}

func mismatch(f FrameResult, field string, want, got any) *CompareMismatch {
	return &CompareMismatch{Frame: f.Frame, SweepID: f.SweepID, Field: field, Want: fmt.Sprint(want), Got: fmt.Sprint(got)}
}

func firstFrame(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}
