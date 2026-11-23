package tactical

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

type EngineOptions struct {
	Search                   SearchOptions
	Smoothing                SmoothingOptions
	Simulation               SimulationOptions
	MaxParallel              int
	StateBufferLimit         int
	SkipConsistencyCheck     bool
	SkipFullEditVerification bool
	Clock                    func() time.Time
}

type Engine struct {
	mu       sync.Mutex
	scenario Scenario
	mapHash  string
	options  EngineOptions
	snapshot *Snapshot
}

func NewEngine(s Scenario, options EngineOptions) (*Engine, error) {
	if err := ValidateScenario(&s); err != nil {
		return nil, err
	}
	if options.Search.HeuristicWeightMilli == 0 &&
		options.Search.Neighbors == 0 &&
		options.Search.MaxExpansions == 0 &&
		options.Search.MaxCost == 0 &&
		options.Search.Heuristic == nil {
		options.Search.HeuristicWeightMilli = -1
	}
	if options.Smoothing.MaxSpacingMilli <= 0 {
		options.Smoothing.MaxSpacingMilli = s.Formation.MaxSpacingMilli
	}
	if options.Smoothing.FormationSamples <= 1 {
		options.Smoothing.FormationSamples = DefaultFormationSamples
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.MaxParallel <= 0 {
		options.MaxParallel = 1
	}
	if options.StateBufferLimit <= 0 {
		options.StateBufferLimit = 4
	}
	return &Engine{
		scenario: cloneScenario(s),
		mapHash:  MapContentHash(s.Map),
		options:  options,
	}, nil
}

func (e *Engine) CheckpointKey() CheckpointKey {
	e.mu.Lock()
	defer e.mu.Unlock()
	return CheckpointKey{
		MapHash:          e.mapHash,
		SlotVersion:      e.scenario.Formation.SlotVersion,
		FixedVersion:     FixedParamsVersion,
		AlgorithmVersion: AlgorithmVersion,
	}
}

func (e *Engine) Scenario() Scenario {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneScenario(e.scenario)
}

func (e *Engine) EffectiveMap() MapDef {
	e.mu.Lock()
	defer e.mu.Unlock()
	return EffectiveMap(e.scenario.Map)
}

func (e *Engine) MapHash() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mapHash
}

func (e *Engine) Snapshot() *Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snapshot == nil {
		return nil
	}
	out := cloneSnapshot(e.snapshot)
	return &out
}

func (e *Engine) FullRebuild(ctx context.Context) (*Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, err := e.buildFull(ctx, e.scenario, "full")
	if err != nil {
		return nil, err
	}
	e.snapshot = snapshot
	out := cloneSnapshot(snapshot)
	return &out, nil
}

func (e *Engine) buildFull(ctx context.Context, scenario Scenario, mode string) (*Snapshot, error) {
	started := e.options.Clock()
	paths, timings, err := e.buildAllPaths(ctx, scenario)
	if err != nil {
		return nil, err
	}
	smooth := make(map[string]SmoothedPath, len(scenario.Formation.Members))
	for _, member := range scenario.Formation.Members {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slot, _ := SlotByID(scenario, member.SlotID)
		path := paths[member.ID]
		pathStart := time.Now()
		result, err := SmoothPath(EffectiveMap(scenario.Map), member, slot, path, e.options.Smoothing)
		timing := timings[member.ID]
		timing.SmoothMicros = time.Since(pathStart).Microseconds()
		timings[member.ID] = timing
		if err != nil {
			return nil, err
		}
		smooth[member.ID] = *result
	}
	if err := CheckFormation(scenario, smooth, e.options.Smoothing); err != nil {
		return nil, err
	}
	traceStart := time.Now()
	var traces map[string]Trace
	if e.options.MaxParallel > 1 {
		traces, _, err = SimulateFormationParallel(ctx, scenario, smooth, e.options.Simulation, e.options.MaxParallel, e.options.StateBufferLimit)
	} else {
		traces, err = SimulateFormation(scenario, smooth, e.options.Simulation)
	}
	traceMicros := time.Since(traceStart).Microseconds()
	if err != nil {
		return nil, err
	}
	if len(scenario.Formation.Members) > 0 {
		timing := timings[scenario.Formation.Members[0].ID]
		timing.TraceMicros = traceMicros
		timings[scenario.Formation.Members[0].ID] = timing
	}
	deps := BuildDependencyGraph(scenario, paths, smooth, traces)
	order := SortedMemberIDs(scenario)
	orderedTimings := make([]MemberTiming, 0, len(order))
	for _, id := range order {
		orderedTimings = append(orderedTimings, timings[id])
	}
	snapshot := &Snapshot{
		MapHash:          MapContentHash(scenario.Map),
		SlotVersion:      scenario.Formation.SlotVersion,
		FixedVersion:     FixedParamsVersion,
		AlgorithmVersion: AlgorithmVersion,
		MemberOrder:      order,
		Paths:            paths,
		Smooth:           smooth,
		Traces:           traces,
		Dependencies:     deps,
		Stats: RunStats{
			Mode: mode, CacheMisses: len(order),
			MaxParallel: e.options.MaxParallel, Timings: orderedTimings,
			TotalMicros: time.Since(started).Microseconds(),
		},
	}
	return snapshot, nil
}

func (e *Engine) buildAllPaths(ctx context.Context, scenario Scenario) (map[string]MemberPath, map[string]MemberTiming, error) {
	order, err := TopoMemberOrder(scenario)
	if err != nil {
		return nil, nil, err
	}
	paths := make(map[string]MemberPath, len(order))
	timings := make(map[string]MemberTiming, len(order))
	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		member, _ := MemberByID(scenario, id)
		start := time.Now()
		path, err := SearchMember(scenario, member, e.options.Search)
		micros := time.Since(start).Microseconds()
		timings[id] = MemberTiming{MemberID: id, PathMicros: micros}
		if err != nil {
			return nil, nil, err
		}
		paths[id] = *path
	}
	return paths, timings, nil
}

func (e *Engine) ApplyCostEdit(ctx context.Context, point GridPoint, cost int64) (*EditResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applyMutationLocked(ctx, func(s *Scenario) error {
		return SetCellCost(&s.Map, point, cost)
	})
}

func (e *Engine) ApplyRegionEdit(ctx context.Context, regionID string, active bool) (*EditResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applyMutationLocked(ctx, func(s *Scenario) error {
		return SetRegionActive(&s.Map, regionID, active)
	})
}

func (e *Engine) applyMutationLocked(ctx context.Context, mutate func(*Scenario) error) (*EditResult, error) {
	if e.snapshot == nil {
		snapshot, err := e.buildFull(ctx, e.scenario, "full")
		if err != nil {
			return nil, err
		}
		e.snapshot = snapshot
	}
	oldScenario := cloneScenario(e.scenario)
	newScenario := cloneScenario(e.scenario)
	if err := mutate(&newScenario); err != nil {
		return nil, err
	}
	newMap := EffectiveMap(newScenario.Map)
	changes := changedMapCells(EffectiveMap(oldScenario.Map), newMap)
	if len(changes) == 0 {
		result := &EditResult{
			Snapshot:    cloneSnapshotPointer(e.snapshot),
			Consistency: ConsistencyResult{Equal: true},
			Stats:       e.snapshot.Stats,
		}
		return result, nil
	}
	report, pathInvalid := PlanInvalidation(oldScenario, newScenario, e.snapshot)
	candidate, err := e.incrementalSnapshot(ctx, newScenario, report, pathInvalid)
	if err != nil {
		return nil, err
	}
	consistency := ConsistencyResult{Equal: true}
	if !e.options.SkipConsistencyCheck && !e.options.SkipFullEditVerification {
		full, err := e.buildFull(ctx, newScenario, "full-verify")
		if err != nil {
			return nil, err
		}
		if err := CompareSnapshots(candidate, full); err != nil {
			consistency.Equal = false
			if typed, ok := AsError(err); ok {
				consistency.Item = typed.Item
				consistency.MemberID = typed.MemberID
				consistency.Tick = typed.Tick
				consistency.Incremental = typed.Message
			}
			return nil, err
		}
		candidate.Invalidation = append([]Invalidation(nil), report.Entries...)
	}
	e.scenario = newScenario
	e.mapHash = MapContentHash(newScenario.Map)
	e.snapshot = candidate
	report.CacheHits = candidate.Stats.CacheHits
	report.CacheMisses = candidate.Stats.CacheMisses
	report.ReusedIntervals = candidate.Stats.ReusedIntervals
	return &EditResult{
		Snapshot: cloneSnapshotPointer(candidate), Invalidation: report,
		Consistency: consistency, Stats: candidate.Stats,
	}, nil
}

func (e *Engine) incrementalSnapshot(ctx context.Context, scenario Scenario, report InvalidationReport, pathInvalid []string) (*Snapshot, error) {
	started := e.options.Clock()
	candidate := cloneSnapshotPointer(e.snapshot)
	candidate.MapHash = MapContentHash(scenario.Map)
	candidate.SlotVersion = scenario.Formation.SlotVersion
	candidate.FixedVersion = FixedParamsVersion
	candidate.AlgorithmVersion = AlgorithmVersion
	candidate.Invalidation = append([]Invalidation(nil), report.Entries...)
	candidate.Stats = RunStats{
		Mode: "incremental", MaxParallel: 1,
	}
	pathSet := make(map[string]struct{}, len(pathInvalid))
	for _, id := range pathInvalid {
		pathSet[id] = struct{}{}
	}
	topo, err := TopoMemberOrder(scenario)
	if err != nil {
		return nil, err
	}
	timings := make(map[string]MemberTiming, len(scenario.Formation.Members))
	for _, id := range topo {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, invalid := pathSet[id]; !invalid {
			candidate.Stats.CacheHits++
			continue
		}
		member, _ := MemberByID(scenario, id)
		pathStart := time.Now()
		path, err := SearchMember(scenario, member, e.options.Search)
		timing := MemberTiming{MemberID: id, PathMicros: time.Since(pathStart).Microseconds()}
		if err != nil {
			return nil, err
		}
		candidate.Paths[id] = *path
		candidate.Stats.CacheMisses++
		slot, _ := SlotByID(scenario, member.SlotID)
		smoothStart := time.Now()
		smooth, err := SmoothPath(EffectiveMap(scenario.Map), member, slot, *path, e.options.Smoothing)
		timing.SmoothMicros = time.Since(smoothStart).Microseconds()
		if err != nil {
			return nil, err
		}
		candidate.Smooth[id] = *smooth
		timings[id] = timing
	}
	if err := CheckFormation(scenario, candidate.Smooth, e.options.Smoothing); err != nil {
		return nil, err
	}
	simInvalid := make([]string, 0)
	simSet := make(map[string]struct{})
	for _, entry := range report.Entries {
		if entry.Kind != InvalidateSimulation {
			continue
		}
		if _, seen := simSet[entry.MemberID]; seen {
			continue
		}
		simSet[entry.MemberID] = struct{}{}
		simInvalid = append(simInvalid, entry.MemberID)
	}
	order := SortedMemberIDs(scenario)
	position := make(map[string]int, len(order))
	for i, id := range order {
		position[id] = i
	}
	sort.Slice(simInvalid, func(i, j int) bool { return position[simInvalid[i]] < position[simInvalid[j]] })
	traceStart := time.Now()
	if e.options.MaxParallel > 1 {
		if _, err := simulateMemberSetParallel(ctx, scenario, candidate.Smooth, simInvalid, candidate.Traces,
			e.options.Simulation, e.options.MaxParallel, e.options.StateBufferLimit); err != nil {
			return nil, err
		}
	} else if err := simulateMemberSet(scenario, candidate.Smooth, simInvalid, candidate.Traces, e.options.Simulation); err != nil {
		return nil, err
	}
	traceMicros := time.Since(traceStart).Microseconds()
	if len(simInvalid) > 0 {
		timing := timings[simInvalid[0]]
		timing.TraceMicros = traceMicros
		timings[simInvalid[0]] = timing
	}
	candidate.Stats.ReusedIntervals = len(order) - len(simInvalid)
	candidate.Stats.Invalidated = len(report.Entries)
	candidate.Stats.MaxParallel = e.options.MaxParallel
	candidate.Dependencies = BuildDependencyGraph(scenario, candidate.Paths, candidate.Smooth, candidate.Traces)
	orderedTimings := make([]MemberTiming, 0, len(order))
	for _, id := range order {
		orderedTimings = append(orderedTimings, timings[id])
	}
	candidate.Stats.Timings = orderedTimings
	candidate.Stats.TotalMicros = time.Since(started).Microseconds()
	candidate.CommitSeq = e.snapshot.CommitSeq + 1
	return candidate, nil
}

func (e *Engine) SaveCheckpoint(ctx context.Context, store *CheckpointStore) (CheckpointMeta, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snapshot == nil {
		snapshot, err := e.buildFull(ctx, e.scenario, "full")
		if err != nil {
			return CheckpointMeta{}, err
		}
		e.snapshot = snapshot
	}
	e.snapshot.SavedAt = e.options.Clock().UTC()
	meta, err := store.Save(ctx, e.snapshot)
	if err != nil {
		return CheckpointMeta{}, err
	}
	e.snapshot.CommitSeq = meta.CommitSeq
	e.snapshot.SavedAt = meta.SavedAt
	return meta, nil
}

func (e *Engine) LoadCheckpoint(ctx context.Context, store *CheckpointStore) (CheckpointMeta, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, meta, err := store.Load(ctx, CheckpointKey{
		MapHash:          e.mapHash,
		SlotVersion:      e.scenario.Formation.SlotVersion,
		FixedVersion:     FixedParamsVersion,
		AlgorithmVersion: AlgorithmVersion,
	})
	if err != nil {
		return meta, err
	}
	if snapshot == nil {
		e.snapshot = nil
		return meta, nil
	}
	if err := validateLoadedSnapshot(e.scenario, snapshot); err != nil {
		e.snapshot = nil
		return meta, err
	}
	e.snapshot = snapshot
	return meta, nil
}

// ResumeOrRebuild loads a matching checkpoint, or discards stale data and
// rebuilds from an empty cache. It is safe to call after process restart.
func (e *Engine) ResumeOrRebuild(ctx context.Context, store *CheckpointStore) (CheckpointMeta, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, meta, err := store.Load(ctx, CheckpointKey{
		MapHash:          e.mapHash,
		SlotVersion:      e.scenario.Formation.SlotVersion,
		FixedVersion:     FixedParamsVersion,
		AlgorithmVersion: AlgorithmVersion,
	})
	if err != nil {
		return meta, false, err
	}
	if snapshot != nil {
		if err := validateLoadedSnapshot(e.scenario, snapshot); err != nil {
			e.snapshot = nil
			return meta, false, err
		}
		e.snapshot = snapshot
		return meta, true, nil
	}
	e.snapshot = nil
	rebuilt, err := e.buildFull(ctx, e.scenario, "full")
	if err != nil {
		return meta, false, err
	}
	rebuilt.SavedAt = e.options.Clock().UTC()
	savedMeta, err := store.Save(ctx, rebuilt)
	if err != nil {
		return meta, false, err
	}
	rebuilt.CommitSeq = savedMeta.CommitSeq
	rebuilt.SavedAt = savedMeta.SavedAt
	e.snapshot = rebuilt
	return savedMeta, false, nil
}

func validateLoadedSnapshot(scenario Scenario, snapshot *Snapshot) error {
	if snapshot.MapHash != MapContentHash(scenario.Map) {
		return schemaError("checkpoint.map_hash", "loaded snapshot map hash does not match current scenario")
	}
	if snapshot.SlotVersion != scenario.Formation.SlotVersion {
		return schemaError("checkpoint.slot_version", "loaded snapshot slot version does not match")
	}
	if snapshot.AlgorithmVersion != AlgorithmVersion || snapshot.FixedVersion != FixedParamsVersion {
		return schemaError("checkpoint.version", "loaded snapshot algorithm or fixed-point version does not match")
	}
	order := SortedMemberIDs(scenario)
	if len(snapshot.MemberOrder) != len(order) {
		return schemaError("checkpoint.member_order", "loaded snapshot member count does not match")
	}
	for i, id := range order {
		if snapshot.MemberOrder[i] != id {
			return schemaError("checkpoint.member_order", "loaded snapshot member order does not match")
		}
		if _, ok := snapshot.Paths[id]; !ok {
			return schemaError("checkpoint.paths", "loaded snapshot is missing path for member "+id)
		}
		if _, ok := snapshot.Smooth[id]; !ok {
			return schemaError("checkpoint.smooth", "loaded snapshot is missing corners for member "+id)
		}
		if _, ok := snapshot.Traces[id]; !ok {
			return schemaError("checkpoint.traces", "loaded snapshot is missing trace for member "+id)
		}
	}
	return nil
}

func cloneScenario(s Scenario) Scenario {
	payload, _ := json.Marshal(s)
	var out Scenario
	_ = json.Unmarshal(payload, &out)
	return out
}

func cloneSnapshotPointer(snapshot *Snapshot) *Snapshot {
	if snapshot == nil {
		return nil
	}
	copySnapshot := cloneSnapshot(snapshot)
	return &copySnapshot
}

func (e *Engine) DiagnosticSummary() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	summary := map[string]any{
		"map_hash": e.mapHash,
		"members":  len(e.scenario.Formation.Members),
	}
	if e.snapshot != nil {
		summary["commit_seq"] = e.snapshot.CommitSeq
		summary["mode"] = e.snapshot.Stats.Mode
		summary["cache_hits"] = e.snapshot.Stats.CacheHits
		summary["cache_misses"] = e.snapshot.Stats.CacheMisses
		summary["invalidations"] = len(e.snapshot.Invalidation)
	}
	return summary
}

func (e *Engine) RetryMember(ctx context.Context, memberID string) (*Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snapshot == nil {
		return nil, fmt.Errorf("no snapshot; run full rebuild first")
	}
	member, ok := MemberByID(e.scenario, memberID)
	if !ok {
		return nil, schemaError("member_id", "unknown member "+memberID)
	}
	path, err := SearchMember(e.scenario, member, e.options.Search)
	if err != nil {
		return nil, err
	}
	slot, _ := SlotByID(e.scenario, member.SlotID)
	smooth, err := SmoothPath(EffectiveMap(e.scenario.Map), member, slot, *path, e.options.Smoothing)
	if err != nil {
		return nil, err
	}
	candidate := cloneSnapshotPointer(e.snapshot)
	candidate.Paths[memberID] = *path
	candidate.Smooth[memberID] = *smooth
	if err := CheckFormation(e.scenario, candidate.Smooth, e.options.Smoothing); err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, other := range e.scenario.Formation.Members {
		if other.Group == member.Group {
			ids = append(ids, other.ID)
		}
	}
	if e.options.MaxParallel > 1 {
		if _, err := simulateMemberSetParallel(ctx, e.scenario, candidate.Smooth, ids, candidate.Traces,
			e.options.Simulation, e.options.MaxParallel, e.options.StateBufferLimit); err != nil {
			return nil, err
		}
	} else if err := simulateMemberSet(e.scenario, candidate.Smooth, ids, candidate.Traces, e.options.Simulation); err != nil {
		return nil, err
	}
	candidate.Dependencies = BuildDependencyGraph(e.scenario, candidate.Paths, candidate.Smooth, candidate.Traces)
	candidate.Stats.Mode = "retry"
	candidate.Stats.CacheHits = len(e.scenario.Formation.Members) - 1
	candidate.Stats.CacheMisses = 1
	candidate.Stats.MaxParallel = e.options.MaxParallel
	e.snapshot = candidate
	return cloneSnapshotPointer(candidate), nil
}
