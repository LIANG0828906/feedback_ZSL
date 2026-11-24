package wb

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
)

type recomputePlan struct {
	reason          string
	dirtyRooms      map[string]string
	targetListeners []string
	items           []InvalidationItem
}

type pairJob struct {
	index      int
	sourceID   string
	listenerID string
	taskID     string
}

func (e *Engine) RunFull(ctx context.Context, resetPersisted bool) (*InvalidationReport, *APIError) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if resetPersisted {
		if e.store != nil {
			if err := e.store.ResetCheckpoints(ctx); err != nil {
				return nil, NewAPIError(CodeInternal, "sqlite", "", "reset checkpoints: %v", err)
			}
		}
		e.results = map[string]ListenerResult{}
		e.checkpoints = map[string]CheckpointData{}
		for id, state := range e.states {
			order := state.CommitOrder
			e.states[id] = &RoomState{
				RoomID: id, Status: StatusPending, CommitOrder: order,
				ParamVersion: ParamVersion, AlgorithmVersion: AlgorithmVersion,
			}
		}
	}

	roomIDs := make([]string, 0, len(e.sample.Rooms))
	dirty := map[string]string{}
	for _, room := range e.sortedRoomsLocked() {
		roomIDs = append(roomIDs, room.ID)
		dirty[room.ID] = "full-recompute-from-empty-cache"
	}
	listenerIDs := make([]string, 0, len(e.sample.Listeners))
	for _, listener := range e.sample.Listeners {
		listenerIDs = append(listenerIDs, listener.ID)
	}
	plan := &recomputePlan{
		reason:          "full-recompute",
		dirtyRooms:      dirty,
		targetListeners: listenerIDs,
	}
	return e.recomputeLocked(ctx, plan)
}

func (e *Engine) recomputeLocked(ctx context.Context, plan *recomputePlan) (*InvalidationReport, *APIError) {
	if plan == nil {
		plan = &recomputePlan{reason: "scheduled-recompute", dirtyRooms: map[string]string{}}
	}
	if plan.dirtyRooms == nil {
		plan.dirtyRooms = map[string]string{}
	}
	e.lastDiff = nil

	targetSet := map[string]bool{}
	for _, id := range plan.targetListeners {
		targetSet[id] = true
		roomID, apiErr := RoomAtPoint(e.sample, e.listenerPoint(id), id, "listener")
		if apiErr != nil {
			return nil, apiErr
		}
		if _, ok := plan.dirtyRooms[roomID]; !ok {
			plan.dirtyRooms[roomID] = "listener-result-dependency"
		}
	}

	commitRoomIDs := sortedKeys(plan.dirtyRooms)
	for _, roomID := range commitRoomIDs {
		state := e.states[roomID]
		if state == nil {
			return nil, NewAPIError(CodeSchema, roomID, "roomId", "unknown room in recompute plan")
		}
		if state.Status == StatusCommitted {
			if apiErr := e.transitionLocked(roomID, StatusPending); apiErr != nil {
				return nil, apiErr
			}
		}
		if state.Status == StatusFailed {
			if apiErr := e.transitionLocked(roomID, StatusPending); apiErr != nil {
				return nil, apiErr
			}
		}
		if apiErr := e.transitionLocked(roomID, StatusAnalyzing); apiErr != nil {
			return nil, apiErr
		}
	}

	jobs := e.buildPairJobsLocked(plan.targetListeners)
	graph := BuildGraph(e.sample)
	pathByJob := make([]*PathResult, len(jobs))
	pathErrByJob := make([]*APIError, len(jobs))
	pathIDs := make([]string, len(jobs))
	for i, job := range jobs {
		pathIDs[i] = "path:" + job.taskID
	}
	e.sched.Run(pathIDs, func(index int) *APIError {
		job := jobs[index]
		path, apiErr := SolvePairPath(e.sample, graph, job.sourceID, job.listenerID)
		if apiErr != nil {
			pathErrByJob[index] = apiErr
		} else {
			pathByJob[index] = path
		}
		return nil
	})

	for _, roomID := range commitRoomIDs {
		if apiErr := e.transitionLocked(roomID, StatusSolving); apiErr != nil {
			return nil, apiErr
		}
	}

	pairResults := make([]PairResult, len(jobs))
	curveIDs := make([]string, len(jobs))
	for i, job := range jobs {
		curveIDs[i] = "curve:" + job.taskID
	}
	taskErrors := e.sched.Run(curveIDs, func(index int) *APIError {
		job := jobs[index]
		if pathErrByJob[index] != nil {
			apiErr := pathErrByJob[index]
			pairResults[index] = PairResult{Error: apiErr}
			return nil
		}
		path := pathByJob[index]
		listener, _ := FindListener(e.sample, job.listenerID)
		roomID, roomErr := RoomAtPoint(e.sample, Point{X: listener.X, Y: listener.Y}, listener.ID, "listener")
		if roomErr != nil {
			pairResults[index] = PairResult{Error: roomErr}
			return roomErr
		}
		if e.failRooms[roomID] {
			apiErr := NewAPIError(CodeState, roomID, "injection", "injected internal failure while solving room %s", roomID)
			pairResults[index] = PairResult{Error: apiErr}
			return apiErr
		}
		samples, spl := e.synthesizePairScheduled("curve:"+job.taskID, path)
		pair := PairResult{
			Path:          *path,
			Samples:       samples,
			ExpectedSPLDB: spl,
		}
		e.annotateTruthLocked(&pair)
		pairResults[index] = pair
		return nil
	})
	e.failRooms = map[string]bool{}

	failedRooms := map[string]*APIError{}
	for _, apiErr := range taskErrors {
		if apiErr == nil {
			continue
		}
		if apiErr.Code == CodeState && apiErr.Object != "" {
			failedRooms[apiErr.Object] = apiErr
		}
	}
	for roomID, apiErr := range failedRooms {
		state := e.states[roomID]
		if state != nil && state.Status == StatusSolving {
			_ = e.transitionLocked(roomID, StatusFailed)
			state.LastError = apiErr
		}
	}

	nextResults := make(map[string]ListenerResult, len(e.results)+len(targetSet))
	for id, result := range e.results {
		nextResults[id] = result
	}
	for _, listenerID := range plan.targetListeners {
		listener, ok := FindListener(e.sample, listenerID)
		if !ok {
			continue
		}
		roomID, apiErr := RoomAtPoint(e.sample, Point{X: listener.X, Y: listener.Y}, listener.ID, "listener")
		if apiErr != nil {
			return nil, apiErr
		}
		if failedRooms[roomID] != nil {
			continue
		}
		result := ListenerResult{ListenerID: listenerID, RoomID: roomID}
		for i, job := range jobs {
			if job.listenerID != listenerID {
				continue
			}
			result.Pairs = append(result.Pairs, pairResults[i])
		}
		nextResults[listenerID] = result
	}

	sampleJSON, err := jsonMarshalSample(e.sample)
	if err != nil {
		return nil, NewAPIError(CodeInternal, "sample", "", "marshal checkpoint sample: %v", err)
	}
	inputHash := InputFingerprint(e.sample)
	nextCheckpoints := make(map[string]CheckpointData, len(e.checkpoints)+len(commitRoomIDs))
	for id, data := range e.checkpoints {
		nextCheckpoints[id] = data
	}

	committedRooms := make([]string, 0, len(commitRoomIDs))
	var firstErr *APIError
	for _, roomID := range commitRoomIDs {
		if failedRooms[roomID] != nil {
			if firstErr == nil {
				firstErr = failedRooms[roomID]
			}
			continue
		}
		room, _ := FindRoom(e.sample, roomID)
		data := CheckpointData{
			RoomID:   roomID,
			Rect:     room.Rect,
			Params:   room.Params,
			Openings: e.openingsForRoomLocked(roomID),
		}
		for _, listener := range e.sample.Listeners {
			result, ok := nextResults[listener.ID]
			if !ok || result.RoomID != roomID {
				continue
			}
			data.ListenerResults = append(data.ListenerResults, cloneListenerResult(result))
			for _, pair := range result.Pairs {
				if pair.Error != nil || pair.Path.SourceID == "" {
					continue
				}
				data.PathSummaries = append(data.PathSummaries, SummarizePath(pair.Path))
			}
		}
		sort.Slice(data.PathSummaries, func(i, j int) bool {
			if data.PathSummaries[i].ListenerID != data.PathSummaries[j].ListenerID {
				return data.PathSummaries[i].ListenerID < data.PathSummaries[j].ListenerID
			}
			return data.PathSummaries[i].SourceID < data.PathSummaries[j].SourceID
		})

		order := e.states[roomID].CommitOrder
		stored, apiErr := BuildStoredCheckpoint(inputHash, order, data)
		if apiErr != nil {
			return nil, apiErr
		}
		if e.store != nil {
			if err := e.store.SaveCheckpoint(ctx, stored, sampleJSON, e.revision); err != nil {
				storeErr := NewAPIError(CodeInternal, "sqlite:"+roomID, "", "atomic checkpoint write failed: %v", err)
				if state := e.states[roomID]; state != nil {
					_ = e.transitionLocked(roomID, StatusFailed)
					state.LastError = storeErr
				}
				return nil, storeErr
			}
		}
		nextCheckpoints[roomID] = data
		committedRooms = append(committedRooms, roomID)
	}

	e.results = nextResults
	e.checkpoints = nextCheckpoints
	e.inputHash = inputHash
	for _, roomID := range committedRooms {
		state := e.states[roomID]
		if apiErr := e.transitionLocked(roomID, StatusCommitted); apiErr != nil {
			return nil, apiErr
		}
		if data, ok := e.checkpoints[roomID]; ok {
			stored, _ := BuildStoredCheckpoint(inputHash, state.CommitOrder, data)
			state.ContentHash = stored.ContentHash
		}
		state.ParamVersion = ParamVersion
		state.AlgorithmVersion = AlgorithmVersion
	}

	report := e.buildReportLocked(plan, failedRooms)
	e.lastInvalidation = report
	if firstErr == nil {
		for _, pair := range pairResults {
			if pair.Error != nil && pair.Error.Code == CodeConn && firstErr == nil {
				firstErr = pair.Error
			}
		}
	}
	return report, firstErr
}

func (e *Engine) buildPairJobsLocked(listenerIDs []string) []pairJob {
	set := map[string]bool{}
	for _, id := range listenerIDs {
		set[id] = true
	}
	jobs := []pairJob{}
	index := 0
	for _, source := range e.sample.Sources {
		for _, listener := range e.sample.Listeners {
			if len(set) > 0 && !set[listener.ID] {
				continue
			}
			jobs = append(jobs, pairJob{
				index:      index,
				sourceID:   source.ID,
				listenerID: listener.ID,
				taskID:     source.ID + "/" + listener.ID,
			})
			index++
		}
	}
	return jobs
}

func (e *Engine) listenerPoint(id string) Point {
	for _, l := range e.sample.Listeners {
		if l.ID == id {
			return Point{X: l.X, Y: l.Y}
		}
	}
	return Point{}
}

func (e *Engine) openingsForRoomLocked(roomID string) []Opening {
	result := []Opening{}
	for _, o := range e.sample.Openings {
		if o.RoomA == roomID || o.RoomB == roomID {
			result = append(result, o)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (e *Engine) synthesizePairScheduled(taskID string, path *PathResult) ([]CurveSample, Fixed) {
	spec := e.sample.Precision
	accumulator := NewCurveAccumulator(e.sample, path)
	result := make([]CurveSample, 0, spec.SampleCount)
	for start := 0; start < spec.SampleCount; start += e.sched.BufferCap() {
		buf, release := e.sched.AcquireSampleBuffer(taskID)
		end := start + e.sched.BufferCap()
		if end > spec.SampleCount {
			end = spec.SampleCount
		}
		count := end - start
		buf = buf[:count]
		for offset := 0; offset < count; offset++ {
			buf[offset] = accumulator.Next()
		}
		result = append(result, buf...)
		release()
	}
	return result, accumulator.InitialSPL()
}

func (e *Engine) annotateTruthLocked(pair *PairResult) {
	truthPath, ok := FindTruthPath(e.sample.Truth, pair.Path.SourceID, pair.Path.ListenerID)
	if !ok {
		pair.SPLMatch = false
		pair.CurveMatch = false
		pair.FirstMismatch = "truth.missing"
		return
	}
	pair.TruthSPLDB = truthPath.ExpectedSPLDB
	if !stringSlicesEqual(pair.Path.RoomChain, truthPath.RoomChain) {
		pair.SPLMatch = false
		pair.CurveMatch = false
		pair.FirstMismatch = "roomChain"
		return
	}
	if pair.Path.Length != truthPath.Length {
		pair.FirstMismatch = "length"
		pair.CurveMatch = false
		return
	}
	tolerance := e.sample.Precision.ToleranceDB
	if absFixed(pair.ExpectedSPLDB-truthPath.ExpectedSPLDB) > tolerance {
		pair.SPLMatch = false
		pair.FirstMismatch = "expectedSplDb"
	} else {
		pair.SPLMatch = true
	}
	if len(pair.Samples) != len(truthPath.DecaySamplesDB) {
		pair.CurveMatch = false
		if pair.FirstMismatch == "" {
			pair.FirstMismatch = "samples.length"
		}
		return
	}
	for i, sample := range pair.Samples {
		if absFixed(sample.LevelDB-truthPath.DecaySamplesDB[i]) > tolerance {
			pair.CurveMatch = false
			if pair.FirstMismatch == "" {
				pair.FirstMismatch = "samples[" + strconv.Itoa(i) + "].levelDb"
			}
			return
		}
	}
	pair.CurveMatch = true
}

func (e *Engine) buildReportLocked(plan *recomputePlan, failedRooms map[string]*APIError) *InvalidationReport {
	items := append([]InvalidationItem(nil), plan.items...)
	recomputedRooms := map[string]bool{}
	for roomID := range plan.dirtyRooms {
		if failedRooms[roomID] == nil {
			recomputedRooms[roomID] = true
		}
	}
	for i := range items {
		item := &items[i]
		if item.Kind == "room" {
			if failedRooms[item.ID] != nil {
				item.Status = "failed"
				item.Reused = false
			} else if recomputedRooms[item.ID] {
				item.Status = "recomputed-and-committed"
				item.Reused = false
			} else {
				item.Status = "reused-persisted-cache"
				item.Reused = true
			}
		}
		if item.Kind == "listener" {
			roomID := item.RoomID
			if failedRooms[roomID] != nil {
				item.Status = "blocked-by-room-failure"
				item.Reused = true
			} else if containsString(plan.targetListeners, item.ID) {
				item.Status = "invalidated-and-resynthesized"
				item.Reused = false
			} else {
				item.Status = "reused-persisted-cache"
				item.Reused = true
			}
		}
	}
	deps := e.buildDependenciesLocked()
	topo, err := TopologicalOrder(deps)
	if err != nil {
		topo = []string{}
	}
	return &InvalidationReport{Reason: plan.reason, Items: items, TopoOrder: topo}
}

func jsonMarshalSample(s *Sample) ([]byte, error) {
	return json.Marshal(s)
}

func absFixed(v Fixed) Fixed {
	if v < 0 {
		return -v
	}
	return v
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
