package solver

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"

	"levelacoustic/internal/acoustics"
	"levelacoustic/internal/geom"
	"levelacoustic/internal/model"
	"levelacoustic/internal/store"
)

type Config struct {
	Store            *store.Store
	Workers          int
	MaxBuffers       int
	BufferSamples    int
	CheckTruth       bool
	StopAfterCommits int
}

type RestoreReport struct {
	HadRows       int      `json:"had_rows"`
	ReusedRooms   []string `json:"reused_rooms"`
	StaleRooms    []string `json:"stale_rooms"`
	Discarded     bool     `json:"discarded"`
	DiscardReason string   `json:"discard_reason,omitempty"`
}

type Engine struct {
	opMu sync.Mutex
	mu   sync.RWMutex

	cfg         Config
	project     *model.Project
	revision    string
	graph       *geom.Graph
	connector   *Connector
	state       *StateMachine
	checkpoints map[string]model.RoomCheckpoint
	chains      map[string][]model.ChainSummary

	invalidation   *model.InvalidationReport
	restoreReport  RestoreReport
	lastResources  model.ResourceSummary
	lastRecomputed []string
	lastWritten    []string
}

func NewEngine(ctx context.Context, project *model.Project, cfg Config) (*Engine, error) {
	if project == nil {
		return nil, model.SchemaError("project", "project is nil")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.GOMAXPROCS(0)
		if cfg.Workers > 4 {
			cfg.Workers = 4
		}
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.MaxBuffers <= 0 {
		cfg.MaxBuffers = cfg.Workers
	}
	if cfg.BufferSamples <= 0 {
		cfg.BufferSamples = model.DefaultCurveSamples
	}
	if err := model.ValidateProject(project, model.ValidateOptions{RequireTruth: cfg.CheckTruth}); err != nil {
		return nil, err
	}
	if cfg.CheckTruth && !project.Truth.VerifyFingerprint(project) {
		return nil, model.SchemaError("truth.project_fingerprint", "truth fingerprint does not match the provided project")
	}
	clone, err := cloneProject(project)
	if err != nil {
		return nil, err
	}
	revision, err := model.ProjectRevision(clone)
	if err != nil {
		return nil, err
	}
	g, err := geom.Build(clone)
	if err != nil {
		return nil, err
	}
	roomIDs := make([]string, 0, len(clone.Rooms))
	for _, room := range clone.Rooms {
		roomIDs = append(roomIDs, room.ID)
	}
	e := &Engine{
		cfg:           cfg,
		project:       clone,
		revision:      revision,
		graph:         g,
		connector:     NewConnector(g),
		state:         NewStateMachine(roomIDs),
		checkpoints:   make(map[string]model.RoomCheckpoint),
		chains:        make(map[string][]model.ChainSummary),
		lastResources: emptyResources(cfg),
	}
	if err := e.restoreCheckpoint(ctx); err != nil {
		return nil, err
	}
	if cfg.CheckTruth {
		var restoredChains []model.ChainSummary
		for _, cp := range e.checkpoints {
			restoredChains = append(restoredChains, cp.Chains...)
		}
		if err := CompareTruth(e.project, restoredChains); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func emptyResources(cfg Config) model.ResourceSummary {
	return model.ResourceSummary{
		MaxParallel:      cfg.Workers,
		BufferCount:      cfg.MaxBuffers,
		MaxBufferSamples: cfg.BufferSamples,
		WorkerCount:      cfg.Workers,
	}
}

func (e *Engine) restoreCheckpoint(ctx context.Context) error {
	if e.cfg.Store == nil {
		return nil
	}
	rows, err := e.cfg.Store.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("load checkpoints: %w", err)
	}
	e.restoreReport.HadRows = len(rows)
	if len(rows) == 0 {
		return nil
	}

	discard := func(reason string) error {
		if err := e.cfg.Store.PurgeAll(ctx); err != nil {
			return fmt.Errorf("purge invalid checkpoints: %w", err)
		}
		e.checkpoints = make(map[string]model.RoomCheckpoint)
		for _, id := range roomIDs(e.project) {
			_ = e.state.Restore(id, model.StatePending, reason)
		}
		e.restoreReport = RestoreReport{
			HadRows:       len(rows),
			Discarded:     true,
			DiscardReason: reason,
		}
		return nil
	}

	currentRooms := make(map[string]model.Room, len(e.project.Rooms))
	expectedOrder := make(map[string]int, len(e.project.Rooms))
	ids := roomIDs(e.project)
	for i, id := range ids {
		expectedOrder[id] = i + 1
	}
	for _, room := range e.project.Rooms {
		currentRooms[room.ID] = room
	}
	rowsByRoom := make(map[string]store.CheckpointRow, len(rows))
	for _, row := range rows {
		if _, ok := currentRooms[row.RoomID]; !ok {
			return discard(fmt.Sprintf("checkpoint references unknown room %s", row.RoomID))
		}
		if _, dup := rowsByRoom[row.RoomID]; dup {
			return discard(fmt.Sprintf("duplicate checkpoint row for room %s", row.RoomID))
		}
		rowsByRoom[row.RoomID] = row
	}

	valid := make(map[string]model.RoomCheckpoint, len(rows))
	var stale []string
	for _, row := range rows {
		if row.ParameterVersion != model.ParameterVersion {
			return discard(fmt.Sprintf("parameter version mismatch for room %s", row.RoomID))
		}
		if row.AlgorithmVersion != model.AlgorithmVersion {
			return discard(fmt.Sprintf("algorithm version mismatch for room %s", row.RoomID))
		}
		var cp model.RoomCheckpoint
		if err := json.Unmarshal(row.Payload, &cp); err != nil {
			return discard(fmt.Sprintf("decode checkpoint for room %s: %v", row.RoomID, err))
		}
		if cp.RoomID != row.RoomID || cp.ContentHash != row.ContentHash ||
			cp.ParameterVersion != row.ParameterVersion || cp.AlgorithmVersion != row.AlgorithmVersion ||
			cp.CommitOrder != row.CommitOrder {
			return discard(fmt.Sprintf("checkpoint envelope mismatch for room %s", row.RoomID))
		}
		if !cp.VerifyHash() {
			return discard(fmt.Sprintf("content hash mismatch for room %s", row.RoomID))
		}
		if expectedOrder[cp.RoomID] != cp.CommitOrder {
			return discard(fmt.Sprintf("unstable commit order for room %s", cp.RoomID))
		}
		if !sameParams(cp.Params, currentRooms[cp.RoomID]) {
			stale = append(stale, cp.RoomID)
			continue
		}
		currentChains, err := e.chainsForRoom(cp.RoomID)
		if err != nil {
			return err
		}
		currentListeners := listenersInRoom(e.project, e.graph, cp.RoomID)
		if err := validateCheckpointShape(cp, currentRooms[cp.RoomID], e.project.Sources, currentListeners, currentChains, doorsUsedByChains(e.project, currentChains)); err != nil {
			stale = append(stale, cp.RoomID)
			continue
		}
		valid[cp.RoomID] = cp
	}
	for roomID, cp := range valid {
		e.checkpoints[roomID] = cp
		e.chains[roomID] = append([]model.ChainSummary(nil), cp.Chains...)
		_ = e.state.Restore(roomID, model.StateCommitted, "checkpoint_restored")
	}
	e.restoreReport.ReusedRooms = sortedKeys(valid)
	e.restoreReport.StaleRooms = sortedUnique(stale)
	return nil
}

func (e *Engine) ApplyProject(ctx context.Context, next *model.Project, operation string) (*model.InvalidationReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if next == nil {
		return nil, model.SchemaError("project", "project is nil")
	}
	if err := model.ValidateProject(next, model.ValidateOptions{RequireTruth: e.cfg.CheckTruth}); err != nil {
		return nil, err
	}
	if e.cfg.CheckTruth && !next.Truth.VerifyFingerprint(next) {
		return nil, model.SchemaError("truth.project_fingerprint", "truth fingerprint does not match the edited project")
	}
	clone, err := cloneProject(next)
	if err != nil {
		return nil, err
	}
	revision, err := model.ProjectRevision(clone)
	if err != nil {
		return nil, err
	}
	g, err := geom.Build(clone)
	if err != nil {
		return nil, err
	}
	connector := NewConnector(g)
	newChains, err := allChains(clone, g, connector)
	if err != nil {
		return nil, err
	}

	oldRoomIDs := setOf(roomIDs(e.project))
	newRoomIDs := setOf(roomIDs(clone))
	roomSetChanged := !sameSet(oldRoomIDs, newRoomIDs)
	if roomSetChanged {
		if e.cfg.Store != nil {
			if err := e.cfg.Store.PurgeAll(ctx); err != nil {
				return nil, err
			}
		}
	}

	paramsChanged := make(map[string]bool)
	for _, room := range clone.Rooms {
		old, _, ok := model.FindRoom(e.project.Rooms, room.ID)
		if ok && !sameParams(old, room) {
			paramsChanged[room.ID] = true
		}
	}

	report := &model.InvalidationReport{
		Operation: operation,
		Revision:  revision,
		Entries:   make([]model.InvalidationEntry, 0, len(clone.Rooms)),
	}
	invalid := make(map[string]bool)
	if roomSetChanged {
		for id := range e.checkpoints {
			invalid[id] = true
		}
	}
	for _, room := range clone.Rooms {
		prior := e.state.State(room.ID)
		cp, hasCP := e.checkpoints[room.ID]
		entry := model.InvalidationEntry{
			RoomID:         room.ID,
			PriorState:     prior,
			ResultingState: prior,
		}
		if !hasCP || roomSetChanged {
			entry.Reason = "not_committed"
			if roomSetChanged {
				entry.Reason = "room_set_changed"
				entry.Invalidated = hasCP
			}
			report.Entries = append(report.Entries, entry)
			continue
		}
		oldMap := chainMap(cp.Chains)
		newMap := chainMap(newChains[room.ID])
		var affected []string
		reasonSet := make(map[string]bool)
		dependencyChanged := false
		if !sameSourceSet(cp.Sources, clone.Sources) {
			dependencyChanged = true
			reasonSet["source_dependency_changed"] = true
		}
		if !sameListenerSet(cp.Listeners, listenersInRoom(clone, g, room.ID)) {
			dependencyChanged = true
			reasonSet["listener_dependency_changed"] = true
		}
		if !sameDoorSet(cp.Doors, doorsUsedByChains(clone, newChains[room.ID])) {
			dependencyChanged = true
			reasonSet["door_snapshot_changed"] = true
		}
		for pair, oldChain := range oldMap {
			newChain, ok := newMap[pair]
			if !ok || !sameChain(oldChain, newChain) {
				affected = append(affected, pair)
				reasonSet["connectivity_changed"] = true
				continue
			}
			for roomID := range paramsChanged {
				if contains(oldChain.RoomChain, roomID) || contains(newChain.RoomChain, roomID) {
					affected = append(affected, pair)
					reasonSet["room_parameter_changed"] = true
					break
				}
			}
		}
		for pair, newChain := range newMap {
			if _, ok := oldMap[pair]; ok {
				continue
			}
			affected = append(affected, pair)
			reasonSet["pair_set_changed"] = true
			for roomID := range paramsChanged {
				if contains(newChain.RoomChain, roomID) {
					reasonSet["room_parameter_changed"] = true
				}
			}
		}
		if dependencyChanged {
			for pair := range newMap {
				affected = append(affected, pair)
			}
			for pair := range oldMap {
				affected = append(affected, pair)
			}
		}
		affected = sortedUnique(affected)
		entry.AffectedPairs = affected
		if len(affected) > 0 {
			invalid[room.ID] = true
			entry.Invalidated = true
			entry.Reason = joinReasons(reasonSet)
			entry.ResultingState = model.StatePending
			if err := e.state.InvalidateForRevision(room.ID, entry.Reason); err != nil {
				return nil, err
			}
		} else {
			entry.Reused = true
			entry.Reason = "dependency_unchanged"
			entry.ResultingState = model.StateCommitted
		}
		report.Entries = append(report.Entries, entry)
	}
	e.mu.Lock()
	e.project = clone
	e.revision = revision
	e.graph = g
	e.connector = connector
	e.chains = newChains
	for id := range invalid {
		delete(e.checkpoints, id)
	}
	e.invalidation = report
	e.lastResources = emptyResources(e.cfg)
	e.lastRecomputed = nil
	e.lastWritten = nil
	e.mu.Unlock()
	report.ReusedRooms = nil
	report.InvalidatedRooms = nil
	for _, entry := range report.Entries {
		if entry.Reused {
			report.ReusedRooms = append(report.ReusedRooms, entry.RoomID)
		}
		if entry.Invalidated {
			report.InvalidatedRooms = append(report.InvalidatedRooms, entry.RoomID)
		}
	}
	return report, nil
}

func (e *Engine) Solve(ctx context.Context) (*model.SolveResult, error) {
	return e.solve(ctx, nil, e.cfg.StopAfterCommits)
}

func (e *Engine) SolveWithLimit(ctx context.Context, commitLimit int) (*model.SolveResult, error) {
	return e.solve(ctx, nil, commitLimit)
}

func (e *Engine) RetryRoom(ctx context.Context, roomID string) (*model.SolveResult, error) {
	state := e.state.State(roomID)
	if state != model.StateFailed {
		return nil, &model.Error{
			Code:     model.ErrState,
			Message:  fmt.Sprintf("room %s can only be retried from failed, current state %s", roomID, state),
			RoomID:   roomID,
			Expected: model.StateFailed,
			Actual:   state,
		}
	}
	if err := e.state.Transition(roomID, model.StatePending); err != nil {
		return nil, err
	}
	return e.solve(ctx, []string{roomID}, e.cfg.StopAfterCommits)
}

func (e *Engine) solve(ctx context.Context, only []string, commitLimit int) (*model.SolveResult, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	e.mu.RLock()
	project := e.project
	g := e.graph
	connector := e.connector
	revision := e.revision
	e.mu.RUnlock()

	var targets []string
	if only != nil {
		targets = append(targets, only...)
	} else {
		for _, id := range roomIDs(project) {
			state := e.state.State(id)
			if state == model.StatePending || state == model.StateFailed {
				targets = append(targets, id)
			}
		}
	}
	sort.Strings(targets)
	if len(targets) == 0 {
		return e.buildResult(nil, nil, model.ResourceSummary{}), nil
	}

	failAll := func(reason string) {
		for _, id := range targets {
			e.state.Fail(id, reason)
		}
	}

	for _, id := range targets {
		if e.state.State(id) == model.StateFailed {
			if err := e.state.Transition(id, model.StatePending); err != nil {
				return nil, err
			}
		}
		if err := e.state.Transition(id, model.StateAnalyzingConnectivity); err != nil {
			return nil, err
		}
	}

	roomChains := make(map[string][]model.ChainSummary, len(targets))
	var allTargetChains []model.ChainSummary
	for _, id := range targets {
		chains, err := e.chainsForRoomWith(id, project, g, connector)
		if err != nil {
			failAll("connectivity_analysis_failed")
			return nil, err
		}
		roomChains[id] = chains
		allTargetChains = append(allTargetChains, chains...)
	}
	if e.cfg.CheckTruth {
		if err := CompareTruth(project, allTargetChains); err != nil {
			failAll("truth_chain_mismatch")
			return nil, err
		}
	}
	for _, id := range targets {
		if err := e.state.Transition(id, model.StateSolvingParameters); err != nil {
			failAll("state_transition_failed")
			return nil, err
		}
	}

	jobs, outputs, err := e.renderRooms(ctx, project, g, roomChains)
	if err != nil {
		failAll("worker_scheduling_failed")
		return nil, err
	}
	_ = jobs

	resultsByRoom := make(map[string][]model.ListenerResult, len(targets))
	for _, result := range outputs {
		resultsByRoom[result.ListenerRoomID] = append(resultsByRoom[result.ListenerRoomID], result)
	}
	for id := range resultsByRoom {
		sort.Slice(resultsByRoom[id], func(i, j int) bool {
			return model.PairID(resultsByRoom[id][i].SourceID, resultsByRoom[id][i].ListenerID) <
				model.PairID(resultsByRoom[id][j].SourceID, resultsByRoom[id][j].ListenerID)
		})
	}
	for id := range roomChains {
		sort.Slice(roomChains[id], func(i, j int) bool {
			return model.PairID(roomChains[id][i].SourceID, roomChains[id][i].ListenerID) <
				model.PairID(roomChains[id][j].SourceID, roomChains[id][j].ListenerID)
		})
	}
	e.mu.Lock()
	for id, chains := range roomChains {
		e.chains[id] = chains
	}
	e.mu.Unlock()

	staged := make([]model.RoomCheckpoint, 0, len(targets))
	order := commitOrder(project)
	for _, id := range targets {
		room, _, _ := model.FindRoom(project.Rooms, id)
		cp := model.RoomCheckpoint{
			RoomID:           id,
			Revision:         revision,
			Params:           room,
			Doors:            doorsUsedByChains(project, roomChains[id]),
			Sources:          append([]model.Source(nil), project.Sources...),
			Listeners:        listenersInRoom(project, g, id),
			Chains:           roomChains[id],
			Results:          resultsByRoom[id],
			ParameterVersion: model.ParameterVersion,
			AlgorithmVersion: model.AlgorithmVersion,
			CommitOrder:      order[id],
		}
		if err := cp.Seal(); err != nil {
			failAll("checkpoint_seal_failed")
			return nil, err
		}
		if err := validateCheckpointShape(cp, room, project.Sources, listenersInRoom(project, g, id), roomChains[id], doorsUsedByChains(project, roomChains[id])); err != nil {
			failAll("checkpoint_shape_invalid")
			return nil, err
		}
		staged = append(staged, cp)
	}
	sort.Slice(staged, func(i, j int) bool { return staged[i].RoomID < staged[j].RoomID })

	var written []string
	interrupted := false
	for i, cp := range staged {
		if commitLimit > 0 && i >= commitLimit {
			interrupted = true
			break
		}
		if e.cfg.Store != nil {
			if err := e.cfg.Store.CommitRoom(ctx, cp); err != nil {
				failAll("checkpoint_write_failed")
				return nil, err
			}
		}
		e.mu.Lock()
		e.checkpoints[cp.RoomID] = cp
		e.mu.Unlock()
		if err := e.state.Transition(cp.RoomID, model.StateCommitted); err != nil {
			failAll("state_transition_failed")
			return nil, err
		}
		written = append(written, cp.RoomID)
	}
	if interrupted {
		for _, id := range targets {
			if e.state.State(id) != model.StateCommitted {
				e.state.Fail(id, "interrupted_after_committed_checkpoint")
			}
		}
		e.mu.Lock()
		e.lastWritten = append([]string(nil), written...)
		e.lastRecomputed = append([]string(nil), written...)
		e.mu.Unlock()
		result := e.buildResult(written, written, model.ResourceSummary{})
		err := &model.Error{
			Code:    model.ErrInterrupted,
			Message: fmt.Sprintf("stopped after %d committed rooms; restart will continue from checkpoints", len(written)),
		}
		return result, err
	}

	e.mu.Lock()
	e.lastWritten = append([]string(nil), written...)
	e.lastRecomputed = append([]string(nil), targets...)
	e.mu.Unlock()
	return e.buildResult(targets, written, model.ResourceSummary{}), nil
}

func (e *Engine) renderRooms(ctx context.Context, project *model.Project, g *geom.Graph, chains map[string][]model.ChainSummary) ([]string, []model.ListenerResult, error) {
	var jobs []string
	outputs := make([]model.ListenerResult, 0)
	roomByID := make(map[string]model.Room, len(project.Rooms))
	for _, room := range project.Rooms {
		roomByID[room.ID] = room
	}
	type jobRef struct {
		roomID   string
		chain    model.ChainSummary
		listener model.Listener
	}
	var refs []jobRef
	for _, roomID := range sortedKeys(chains) {
		for _, chain := range chains[roomID] {
			listener, _, ok := model.FindPoint(project.Listeners, chain.ListenerID)
			if !ok {
				return nil, nil, model.GeometryError(roomID, "", "listener_id", "unknown listener %s", chain.ListenerID)
			}
			refs = append(refs, jobRef{roomID: roomID, chain: chain, listener: listener})
		}
	}
	outputs = make([]model.ListenerResult, len(refs))
	pool := newWorkerPool(e.cfg.Workers, e.cfg.MaxBuffers, e.cfg.BufferSamples)
	for i := range refs {
		i := i
		ref := refs[i]
		jobID := model.PairID(ref.chain.SourceID, ref.chain.ListenerID)
		jobs = append(jobs, jobID)
		if err := pool.submit(ctx, workerJob{
			id:      jobID,
			samples: model.DefaultCurveSamples,
			run: func(scratch []int64) error {
				result, err := renderOne(project, g, roomByID, ref.roomID, ref.chain, scratch)
				if err != nil {
					return err
				}
				outputs[i] = result
				return nil
			},
		}); err != nil {
			_ = pool.close()
			return jobs, nil, err
		}
	}
	if err := pool.close(); err != nil {
		return jobs, nil, err
	}
	e.mu.Lock()
	e.lastResources = pool.summary()
	e.mu.Unlock()
	return jobs, outputs, nil
}

func renderOne(project *model.Project, g *geom.Graph, roomByID map[string]model.Room, roomID string, chain model.ChainSummary, scratch []int64) (model.ListenerResult, error) {
	result := model.ListenerResult{
		ListenerID:      chain.ListenerID,
		SourceID:        chain.SourceID,
		ListenerRoomID:  chain.ListenerRoomID,
		Reachable:       chain.Reachable,
		RoomChain:       append([]string(nil), chain.RoomChain...),
		DoorChain:       append([]string(nil), chain.DoorChain...),
		PathLengthMicro: chain.PathLengthMicro,
		Segments:        append([]model.TruthSegment(nil), chain.Segments...),
		Truncations:     append([]model.Truncation(nil), chain.Truncations...),
	}
	if !chain.Reachable {
		return result, nil
	}
	chainRooms, err := acoustics.ChainAverageParams(chain.RoomChain, roomByID)
	if err != nil {
		return model.ListenerResult{}, err
	}
	prepared := acoustics.Prepare(acoustics.ChainInput{
		Rooms:           chainRooms,
		ListenerRoom:    roomByID[chain.ListenerRoomID],
		DoorCount:       len(chain.DoorChain),
		PathLengthMicro: chain.PathLengthMicro,
		CurveSamples:    model.DefaultCurveSamples,
		CurveStepMicros: model.DefaultCurveStepMicros,
	})
	result.SPLDBMicro = prepared.SPLDBMicro
	result.OcclusionDBMicro = prepared.OcclusionDBMicro
	result.CurveDBMicro = make([]int64, prepared.CurveSamples)
	result.Truncations = append(result.Truncations, prepared.Truncations...)
	for offset := 0; offset < prepared.CurveSamples; {
		n := len(scratch)
		if n <= 0 {
			return model.ListenerResult{}, model.NewError(model.ErrResource, "zero-sized sample buffer")
		}
		if n > prepared.CurveSamples-offset {
			n = prepared.CurveSamples - offset
		}
		truncs := prepared.RenderInto(scratch[:n], offset)
		copy(result.CurveDBMicro[offset:offset+n], scratch[:n])
		result.Truncations = append(result.Truncations, truncs...)
		offset += n
	}
	return result, nil
}

func (e *Engine) buildResult(recomputed, written []string, resources model.ResourceSummary) *model.SolveResult {
	e.mu.RLock()
	defer e.mu.RUnlock()
	checkpoints := make([]model.RoomCheckpoint, 0, len(e.checkpoints))
	for _, cp := range e.checkpoints {
		checkpoints = append(checkpoints, cloneJSON(cp))
	}
	sort.Slice(checkpoints, func(i, j int) bool { return checkpoints[i].RoomID < checkpoints[j].RoomID })
	var results []model.ListenerResult
	for _, cp := range checkpoints {
		for _, result := range cp.Results {
			results = append(results, cloneJSON(result))
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return model.PairID(results[i].SourceID, results[i].ListenerID) <
			model.PairID(results[j].SourceID, results[j].ListenerID)
	})
	seq := make([]string, 0, len(checkpoints))
	for _, cp := range checkpoints {
		seq = append(seq, cp.RoomID)
	}
	sort.Strings(seq)
	committed := len(checkpoints)
	total := len(e.project.Rooms)
	reused := make([]string, 0)
	for _, cp := range checkpoints {
		if !contains(recomputed, cp.RoomID) {
			reused = append(reused, cp.RoomID)
		}
	}
	if resources.MaxParallel == 0 {
		resources = e.lastResources
	}
	if resources.MaxParallel == 0 {
		resources = emptyResources(e.cfg)
	}
	return &model.SolveResult{
		Revision:        e.revision,
		RoomStates:      e.state.Snapshot(),
		Progress:        fmt.Sprintf("%d/%d", committed, total),
		CommittedRooms:  committed,
		TotalRooms:      total,
		CommitSequence:  seq,
		RecomputedRooms: sortedUnique(recomputed),
		ReusedRooms:     sortedUnique(reused),
		Invalidation:    e.invalidation,
		RoomCheckpoints: checkpoints,
		ListenerResults: results,
		Resources:       resources,
	}
}

func (e *Engine) chainsForRoom(roomID string) ([]model.ChainSummary, error) {
	e.mu.RLock()
	project := e.project
	g := e.graph
	connector := e.connector
	e.mu.RUnlock()
	return e.chainsForRoomWith(roomID, project, g, connector)
}

func (e *Engine) chainsForRoomWith(roomID string, project *model.Project, g *geom.Graph, connector *Connector) ([]model.ChainSummary, error) {
	listeners := listenersInRoom(project, g, roomID)
	var out []model.ChainSummary
	for _, listener := range listeners {
		for _, source := range project.Sources {
			chain, err := connector.ChainSummary(source, listener)
			if err != nil {
				return nil, err
			}
			out = append(out, chain)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return model.PairID(out[i].SourceID, out[i].ListenerID) < model.PairID(out[j].SourceID, out[j].ListenerID)
	})
	return out, nil
}

func allChains(project *model.Project, g *geom.Graph, connector *Connector) (map[string][]model.ChainSummary, error) {
	out := make(map[string][]model.ChainSummary, len(project.Rooms))
	for _, room := range project.Rooms {
		listeners := listenersInRoom(project, g, room.ID)
		var chains []model.ChainSummary
		for _, listener := range listeners {
			for _, source := range project.Sources {
				chain, err := connector.ChainSummary(source, listener)
				if err != nil {
					return nil, err
				}
				chains = append(chains, chain)
			}
		}
		sort.Slice(chains, func(i, j int) bool {
			return model.PairID(chains[i].SourceID, chains[i].ListenerID) < model.PairID(chains[j].SourceID, chains[j].ListenerID)
		})
		out[room.ID] = chains
	}
	return out, nil
}

func listenersInRoom(project *model.Project, g *geom.Graph, roomID string) []model.Listener {
	var out []model.Listener
	for _, listener := range project.Listeners {
		room, ok := g.RoomForPoint(listener.X, listener.Y)
		if ok && room == roomID {
			out = append(out, listener)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func doorsUsedByChains(project *model.Project, chains []model.ChainSummary) []model.Door {
	used := make(map[string]bool)
	for _, chain := range chains {
		for _, doorID := range chain.DoorChain {
			used[doorID] = true
		}
	}
	var out []model.Door
	for _, door := range project.Doors {
		if used[door.ID] {
			out = append(out, door)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func validateCheckpointShape(cp model.RoomCheckpoint, currentRoom model.Room, sources []model.Source, currentListeners []model.Listener, currentChains []model.ChainSummary, currentDoors []model.Door) error {
	if !sameParams(cp.Params, currentRoom) {
		return fmt.Errorf("room parameters changed")
	}
	if !sameSourceSet(cp.Sources, sources) {
		return fmt.Errorf("source set changed")
	}
	if !sameListenerSet(cp.Listeners, currentListeners) {
		return fmt.Errorf("listener set changed")
	}
	if !sameDoorSet(cp.Doors, currentDoors) {
		return fmt.Errorf("door snapshot changed")
	}
	cpChains := chainMap(cp.Chains)
	curChains := chainMap(currentChains)
	if len(cpChains) != len(curChains) {
		return fmt.Errorf("pair count changed")
	}
	for pair, current := range curChains {
		old, ok := cpChains[pair]
		if !ok || !sameChain(old, current) {
			return fmt.Errorf("chain changed for %s", pair)
		}
	}
	cpResults := make(map[string]model.ListenerResult, len(cp.Results))
	for _, result := range cp.Results {
		cpResults[model.PairID(result.SourceID, result.ListenerID)] = result
	}
	if len(cpResults) != len(cpChains) {
		return fmt.Errorf("result pair count mismatch")
	}
	for pair, chain := range cpChains {
		result, ok := cpResults[pair]
		if !ok {
			return fmt.Errorf("missing result for %s", pair)
		}
		if result.ListenerRoomID != chain.ListenerRoomID || result.PathLengthMicro != chain.PathLengthMicro ||
			result.Reachable != chain.Reachable {
			return fmt.Errorf("result/chain mismatch for %s", pair)
		}
		if chain.Reachable && len(result.CurveDBMicro) != model.DefaultCurveSamples {
			return fmt.Errorf("curve sample count mismatch for %s", pair)
		}
	}
	return nil
}

func commitOrder(project *model.Project) map[string]int {
	ids := roomIDs(project)
	out := make(map[string]int, len(ids))
	for i, id := range ids {
		out[id] = i + 1
	}
	return out
}

func roomIDs(project *model.Project) []string {
	out := make([]string, 0, len(project.Rooms))
	for _, room := range project.Rooms {
		out = append(out, room.ID)
	}
	sort.Strings(out)
	return out
}

func sameParams(a, b model.Room) bool {
	return a.ID == b.ID &&
		a.RT60Milliseconds == b.RT60Milliseconds &&
		a.EarlyDensityMilli == b.EarlyDensityMilli &&
		a.ReverbGainMilli == b.ReverbGainMilli
}

func sameSourceSet(a, b []model.Source) bool {
	if len(a) != len(b) {
		return false
	}
	am := make(map[string]model.Source, len(a))
	for _, s := range a {
		am[s.ID] = s
	}
	for _, s := range b {
		old, ok := am[s.ID]
		if !ok || old.X != s.X || old.Y != s.Y {
			return false
		}
	}
	return true
}

func sameListenerSet(a, b []model.Listener) bool {
	if len(a) != len(b) {
		return false
	}
	am := make(map[string]model.Listener, len(a))
	for _, l := range a {
		am[l.ID] = l
	}
	for _, l := range b {
		old, ok := am[l.ID]
		if !ok || old.X != l.X || old.Y != l.Y {
			return false
		}
	}
	return true
}

func sameDoorSet(a, b []model.Door) bool {
	if len(a) != len(b) {
		return false
	}
	am := make(map[string]model.Door, len(a))
	for _, d := range a {
		am[d.ID] = d
	}
	for _, d := range b {
		old, ok := am[d.ID]
		if !ok || old.WallID != d.WallID || old.Start != d.Start || old.Width != d.Width {
			return false
		}
	}
	return true
}

func sameChain(a, b model.ChainSummary) bool {
	if a.Reachable != b.Reachable || a.PathLengthMicro != b.PathLengthMicro ||
		a.ListenerRoomID != b.ListenerRoomID {
		return false
	}
	if !sameStrings(a.RoomChain, b.RoomChain) || !sameStrings(a.DoorChain, b.DoorChain) {
		return false
	}
	if len(a.Segments) != len(b.Segments) {
		return false
	}
	for i := range a.Segments {
		if a.Segments[i].RoomID != b.Segments[i].RoomID ||
			a.Segments[i].LengthMicro != b.Segments[i].LengthMicro {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func chainMap(chains []model.ChainSummary) map[string]model.ChainSummary {
	out := make(map[string]model.ChainSummary, len(chains))
	for _, chain := range chains {
		out[model.PairID(chain.SourceID, chain.ListenerID)] = chain
	}
	return out
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func setOf(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for v := range a {
		if !b[v] {
			return false
		}
	}
	return true
}

func joinReasons(reasons map[string]bool) string {
	out := make([]string, 0, len(reasons))
	for reason := range reasons {
		out = append(out, reason)
	}
	sort.Strings(out)
	return strings.Join(out, "+")
}

func cloneProject(p *model.Project) (*model.Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func cloneJSON[T any](v T) T {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return v
	}
	return out
}

func (e *Engine) Project() *model.Project {
	e.mu.RLock()
	defer e.mu.RUnlock()
	clone, err := cloneProject(e.project)
	if err != nil {
		return nil
	}
	return clone
}

func (e *Engine) Revision() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.revision
}

func (e *Engine) Restore() RestoreReport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneJSON(e.restoreReport)
}

func (e *Engine) LastInvalidation() *model.InvalidationReport {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.invalidation == nil {
		return nil
	}
	out := cloneJSON(*e.invalidation)
	return &out
}

func (e *Engine) LastWritten() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]string(nil), e.lastWritten...)
}

func (e *Engine) Snapshot(ctx context.Context) (*model.SolveResult, *model.Project, RestoreReport, error) {
	e.mu.RLock()
	project, err := cloneProject(e.project)
	restore := cloneJSON(e.restoreReport)
	resources := e.lastResources
	recomputed := append([]string(nil), e.lastRecomputed...)
	e.mu.RUnlock()
	if err != nil {
		return nil, nil, RestoreReport{}, err
	}
	return e.buildResult(recomputed, nil, resources), project, restore, nil
}

func (e *Engine) Reset(ctx context.Context) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.cfg.Store != nil {
		if err := e.cfg.Store.PurgeAll(ctx); err != nil {
			return err
		}
	}
	e.mu.Lock()
	e.checkpoints = make(map[string]model.RoomCheckpoint)
	e.chains = make(map[string][]model.ChainSummary)
	e.invalidation = nil
	e.lastResources = emptyResources(e.cfg)
	e.lastRecomputed = nil
	e.lastWritten = nil
	e.restoreReport = RestoreReport{Discarded: true, DiscardReason: "explicit_full_reset"}
	e.mu.Unlock()
	for _, id := range roomIDs(e.project) {
		if err := e.state.Restore(id, model.StatePending, "full_reset"); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) QueueStats() model.ResourceSummary {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneJSON(e.lastResources)
}

func (e *Engine) StoreStats() (store.StoreStats, bool) {
	if e.cfg.Store == nil {
		return store.StoreStats{}, false
	}
	return e.cfg.Store.Stats(), true
}

func (e *Engine) Store() *store.Store { return e.cfg.Store }
