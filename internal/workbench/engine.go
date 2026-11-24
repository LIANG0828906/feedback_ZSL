package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
)

type Engine struct {
	region      *Region
	state       WorkspaceState
	baseHash    string
	stateHash   string
	snapshotID  string
	options     RuntimeConfig
	store       PersistentStore
	cache       map[string]*ZoneResult
	topology    []string
	zoneOrder   map[string]int
	versionMemo map[string]string
	snapshot    SnapshotStatus
	warnings    []string
}

func NewEngine(region *Region, options RuntimeConfig, store PersistentStore) (*Engine, *Error) {
	if err := ValidateRegion(region); err != nil {
		return nil, err
	}
	normalized := normalizeRegion(region)
	baseHash := baseContentHash(normalized)
	topo, err := topologyOrder(normalized.Zones)
	if err != nil {
		return nil, err
	}
	order := make(map[string]int, len(topo))
	for i, id := range topo {
		order[id] = i
	}
	if options.MaxParallel <= 0 {
		options.MaxParallel = normalized.Sampling.MaxParallel
	}
	if options.MaxParallel > normalized.Sampling.MaxParallel {
		options.MaxParallel = normalized.Sampling.MaxParallel
	}
	if options.BufferLimit <= 0 {
		options.BufferLimit = normalized.Sampling.PerThreadSampleBuffer
	}
	if options.BufferLimit > normalized.Sampling.PerThreadSampleBuffer {
		options.BufferLimit = normalized.Sampling.PerThreadSampleBuffer
	}
	e := &Engine{
		region:      normalized,
		state:       defaultState(normalized),
		baseHash:    baseHash,
		snapshotID:  "snap-" + baseHash[:16],
		options:     options,
		store:       store,
		cache:       make(map[string]*ZoneResult),
		topology:    topo,
		zoneOrder:   order,
		versionMemo: make(map[string]string),
		snapshot: SnapshotStatus{
			Enabled:    store != nil,
			SnapshotID: "snap-" + baseHash[:16],
		},
	}
	e.stateHash = stableDigest(e.state)
	if store != nil {
		e.restoreSnapshot()
	}
	return e, nil
}

func NewEngineForTest(region *Region, options RuntimeConfig) (*Engine, *Error) {
	return NewEngine(region, options, nil)
}

func (e *Engine) Region() *Region {
	return e.region
}

func (e *Engine) State() WorkspaceState {
	return cloneJSON(e.state)
}

func (e *Engine) BaseHash() string  { return e.baseHash }
func (e *Engine) StateHash() string { return e.stateHash }

func (e *Engine) restoreSnapshot() {
	meta, rows, err := e.store.LoadSnapshot(e.snapshotID)
	if err != nil {
		e.snapshot.RestoreError = err.Error()
		e.warnings = append(e.warnings, "snapshot load failed: "+err.Error())
		return
	}
	if meta == nil {
		e.saveDefaultSnapshot()
		return
	}
	if meta.BaseHash != e.baseHash || meta.AlgorithmVersion != AlgorithmVersion || meta.RegionID != e.region.RegionID {
		e.invalidateSnapshot("snapshot content or algorithm version mismatch")
		return
	}
	var restoredState WorkspaceState
	if err := json.Unmarshal([]byte(meta.StateJSON), &restoredState); err != nil {
		e.invalidateSnapshot("snapshot state JSON is invalid")
		return
	}
	if stableDigest(restoredState) != meta.StateHash {
		e.invalidateSnapshot("snapshot state hash mismatch")
		return
	}
	if err := validateState(e.region, &restoredState); err != nil {
		e.invalidateSnapshot("snapshot state failed validation: " + err.Error())
		return
	}
	e.state = normalizeState(restoredState)
	e.stateHash = stableDigest(e.state)
	e.rebuildVersionMemo()
	versions := e.allParamVersions()
	selected := make(map[string]*ZoneResult, len(rows))
	for _, row := range rows {
		expected, ok := versions[row.ZoneID]
		if !ok {
			e.invalidateSnapshot("snapshot contains unknown zone " + row.ZoneID)
			return
		}
		if row.ContentHash != e.baseHash || row.ParamVersion != expected ||
			row.AlgorithmVersion != AlgorithmVersion {
			e.invalidateSnapshot("checkpoint version mismatch for zone " + row.ZoneID)
			return
		}
		var result ZoneResult
		if err := json.Unmarshal([]byte(row.ResultJSON), &result); err != nil {
			e.invalidateSnapshot("checkpoint result is corrupt for zone " + row.ZoneID)
			return
		}
		if result.ZoneID != row.ZoneID || result.ParamVersion != expected ||
			result.PointDigest != stableDigest(result.Points) ||
			result.HeatDigest != stableDigest(result.HeatLayer) ||
			result.PointCount != len(result.Points) {
			e.invalidateSnapshot("checkpoint digest mismatch for zone " + row.ZoneID)
			return
		}
		result.Generation = row.Generation
		result.CommitSeq = row.CommitSeq
		result.TopologyOrder = e.zoneOrder[row.ZoneID]
		selected[row.ZoneID] = &result
	}
	for id, result := range selected {
		e.cache[id] = result
	}
	e.snapshot.Restored = len(selected) > 0
}

func (e *Engine) invalidateSnapshot(reason string) {
	if e.store == nil {
		return
	}
	e.snapshot.Invalidated = true
	e.snapshot.Restored = false
	e.snapshot.RestoreError = reason
	e.warnings = append(e.warnings, "snapshot invalidated: "+reason)
	if err := e.store.DeleteSnapshot(e.snapshotID); err != nil {
		e.warnings = append(e.warnings, "snapshot delete failed: "+err.Error())
	}
	e.cache = make(map[string]*ZoneResult)
	e.state = defaultState(e.region)
	e.stateHash = stableDigest(e.state)
	e.rebuildVersionMemo()
	e.saveDefaultSnapshot()
}

func (e *Engine) saveDefaultSnapshot() {
	if e.store == nil {
		return
	}
	stateJSON, _ := json.Marshal(e.state)
	meta := SnapshotMeta{
		SnapshotID:       e.snapshotID,
		RegionID:         e.region.RegionID,
		BaseHash:         e.baseHash,
		AlgorithmVersion: AlgorithmVersion,
		StateJSON:        string(stateJSON),
		StateHash:        e.stateHash,
	}
	if err := e.store.SaveSnapshot(meta); err != nil {
		e.warnings = append(e.warnings, "snapshot save failed: "+err.Error())
	}
}

func (e *Engine) Run(ctx context.Context) (*RunResult, *Error) {
	if err := ctx.Err(); err != nil {
		return nil, ioError("run cancelled: %v", err)
	}
	missing := make([]string, 0)
	for _, id := range e.topology {
		if e.cache[id] == nil {
			missing = append(missing, id)
		}
	}
	recomputed := make([]string, 0)
	workers := WorkerStats{MaxParallel: e.options.MaxParallel, BufferLimit: e.options.BufferLimit}
	if len(missing) > 0 {
		results, stats, err := e.computeZones(ctx, missing)
		if err != nil {
			return nil, err
		}
		workers = stats
		for _, id := range missing {
			result := results[id]
			if result == nil {
				return nil, ioError("missing result for zone %s", id)
			}
			if err := e.commitResult(result); err != nil {
				return nil, err
			}
			recomputed = append(recomputed, id)
		}
	}
	invalidations := make([]InvalidationEntry, 0, len(e.topology))
	for _, id := range e.topology {
		reused := !containsString(recomputed, id)
		reason := "persistent_checkpoint"
		state := CacheReused
		if !reused {
			reason = "cache_missing"
			if len(missing) == len(e.topology) && !e.snapshot.Restored {
				reason = "cold_cache"
			}
			state = CacheRecomputed
		}
		entry := InvalidationEntry{
			ZoneID: id, Stage: "zone:reconstruction", Invalidated: !reused,
			Reused: reused, Reasons: []string{reason}, CacheState: state,
		}
		if reused && !e.snapshot.Restored {
			entry.Reasons = []string{"session_cache"}
		}
		invalidations = append(invalidations, entry)
	}
	mode := "incremental"
	if len(recomputed) == len(e.topology) {
		mode = "full"
	} else if len(recomputed) == 0 {
		mode = "cache"
	}
	result, err := e.assembleResult(mode, recomputed, invalidations, workers)
	if err != nil {
		return result, err
	}
	return result, nil
}

func (e *Engine) commitResult(result *ZoneResult) *Error {
	if result == nil {
		return ioError("cannot commit nil zone result")
	}
	stateJSON, _ := json.Marshal(e.state)
	if e.store != nil {
		sampleIndices, _ := json.Marshal(result.SampleSelection.Indices)
		estimateJSON, _ := json.Marshal(result.Estimate)
		pointSummary, _ := json.Marshal(map[string]any{
			"count": result.PointCount, "digest": result.PointDigest,
		})
		heatSummary, _ := json.Marshal(map[string]any{
			"cells": len(result.HeatLayer), "digest": result.HeatDigest,
		})
		resultJSON, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return storeError("encode zone result: %v", marshalErr)
		}
		checkpoint := ZoneCheckpoint{
			ZoneID:           result.ZoneID,
			TopologyOrder:    e.zoneOrder[result.ZoneID],
			ContentHash:      e.baseHash,
			ParamVersion:     result.ParamVersion,
			AlgorithmVersion: AlgorithmVersion,
			SampleIndices:    string(sampleIndices),
			EstimateJSON:     string(estimateJSON),
			PointSummary:     string(pointSummary),
			HeatSummary:      string(heatSummary),
			ResultJSON:       string(resultJSON),
			Generation:       result.Generation,
		}
		saved, err := e.store.CommitZone(e.snapshotID, string(stateJSON), e.stateHash, checkpoint)
		if err != nil {
			return storeError("commit checkpoint for zone %s: %v", result.ZoneID, err)
		}
		result.Generation = saved.Generation
		result.CommitSeq = saved.CommitSeq
		e.snapshot.Committed++
	} else {
		generation := 0
		if existing := e.cache[result.ZoneID]; existing != nil {
			generation = existing.Generation
		}
		result.Generation = generation + 1
		result.CommitSeq = int64(result.Generation)
	}
	e.cache[result.ZoneID] = result
	return nil
}

func (e *Engine) assembleResult(
	mode string,
	recomputed []string,
	invalidations []InvalidationEntry,
	workers WorkerStats,
) (*RunResult, *Error) {
	zones := make([]*ZoneResult, 0, len(e.topology))
	for _, id := range e.topology {
		if e.cache[id] == nil {
			return nil, ioError("cache is missing zone %s", id)
		}
		zones = append(zones, e.cache[id])
	}
	bias, biasErr := buildBiasReport(e.region, zones)
	heatmap := mergeHeatmap(e.region, zones)
	points := make([]Point, 0)
	for _, zone := range zones {
		points = append(points, zone.Points...)
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i].ZoneID != points[j].ZoneID {
			return points[i].ZoneID < points[j].ZoneID
		}
		if points[i].X != points[j].X {
			return points[i].X < points[j].X
		}
		if points[i].Y != points[j].Y {
			return points[i].Y < points[j].Y
		}
		return points[i].ID < points[j].ID
	})
	result := &RunResult{
		RegionID:      e.region.RegionID,
		Mode:          mode,
		BaseHash:      e.baseHash,
		StateHash:     e.stateHash,
		Algorithm:     AlgorithmVersion,
		CommitOrder:   append([]string(nil), e.topology...),
		Recomputed:    append([]string(nil), recomputed...),
		Invalidations: invalidations,
		Dependency:    e.dependencyGraph(),
		Zones:         zones,
		Points:        points,
		Heatmap:       heatmap,
		Bias:          bias,
		Worker:        workers,
		Snapshot:      e.snapshot,
		Warnings:      append([]string(nil), e.warnings...),
	}
	return result, biasErr
}

func (e *Engine) dependencyGraph() DependencyGraph {
	nodes := make([]DependencyNode, 0, len(e.topology)*9+len(e.state.NoCollect)+2)
	surfaceDepends := make([]string, 0, len(e.topology))
	nodes = append(nodes, DependencyNode{ID: "global:surface", Kind: "surface", Depends: surfaceDepends})
	nodes = append(nodes, DependencyNode{ID: "global:bias", Kind: "report", Depends: []string{"global:surface"}})
	for _, nc := range e.state.NoCollect {
		id := "no_collect:" + nc.ID
		nodes = append(nodes, DependencyNode{ID: id, Kind: "no_collect_boundary"})
	}
	for _, id := range e.topology {
		zone := findZone(e.region, id)
		paramDeps := []string{"zone:" + id + ":density_limit", "zone:" + id + ":rarity_mix"}
		sampleDeps := []string{}
		for _, nc := range e.state.NoCollect {
			if polygonOverlaps(zone.Polygon, nc.Polygon) {
				paramDeps = append(paramDeps, "no_collect:"+nc.ID)
				sampleDeps = append(sampleDeps, "no_collect:"+nc.ID)
			}
		}
		downstreamDeps := []string{"zone:" + id + ":estimate"}
		for _, upstream := range zone.DependsOn {
			downstreamDeps = append(downstreamDeps, "zone:"+upstream+":downstream")
		}
		nodes = append(nodes,
			DependencyNode{ID: "zone:" + id + ":density_limit", Kind: "density_limit", ZoneID: id},
			DependencyNode{ID: "zone:" + id + ":rarity_mix", Kind: "rarity_mix", ZoneID: id},
			DependencyNode{ID: "zone:" + id + ":params", Kind: "parameters", ZoneID: id, Depends: paramDeps},
			DependencyNode{ID: "zone:" + id + ":samples", Kind: "sample_observations", ZoneID: id, Depends: sampleDeps},
			DependencyNode{ID: "zone:" + id + ":sampling", Kind: "sampling", ZoneID: id, Depends: []string{"zone:" + id + ":params", "zone:" + id + ":samples"}},
			DependencyNode{ID: "zone:" + id + ":estimate", Kind: "estimate", ZoneID: id, Depends: []string{"zone:" + id + ":sampling"}},
			DependencyNode{ID: "zone:" + id + ":reconstruction", Kind: "reconstruction", ZoneID: id, Depends: []string{"zone:" + id + ":estimate", "zone:" + id + ":params"}},
			DependencyNode{ID: "zone:" + id + ":heat", Kind: "heat", ZoneID: id, Depends: []string{"zone:" + id + ":reconstruction"}},
			DependencyNode{ID: "zone:" + id + ":downstream", Kind: "downstream", ZoneID: id, Depends: downstreamDeps},
		)
		surfaceDepends = append(surfaceDepends, "zone:"+id+":heat")
	}
	for i := range nodes {
		if nodes[i].ID == "global:surface" {
			nodes[i].Depends = surfaceDepends
		}
	}
	for i := range nodes {
		sort.Strings(nodes[i].Depends)
	}
	return DependencyGraph{Nodes: nodes, TopologyOrder: append([]string(nil), e.topology...)}
}

func (e *Engine) EditZone(ctx context.Context, req ZoneEditRequest) (*RunResult, *Error) {
	zone := findZone(e.region, req.ZoneID)
	if zone == nil {
		return nil, schemaError("zone_id", "unknown zone %q", req.ZoneID)
	}
	newState := cloneJSON(e.state)
	params := newState.ZoneParams[zone.ID]
	reasons := make([]string, 0)
	if req.DensityLimit != nil {
		if !finite(*req.DensityLimit) || *req.DensityLimit < 0 {
			return nil, schemaError("density_limit", "density_limit must be finite and non-negative")
		}
		if params.DensityLimit != *req.DensityLimit {
			params.DensityLimit = *req.DensityLimit
			reasons = append(reasons, "zone_parameter:density_limit")
		}
	}
	if req.ClearRarityOverride {
		if params.RarityMixOverride != nil {
			params.RarityMixOverride = nil
			reasons = append(reasons, "zone_parameter:rarity_mix")
		}
	} else if req.RarityMix != nil {
		if err := validateMix(req.RarityMix, "rarity_mix"); err != nil {
			return nil, err
		}
		normalized := normalizeMix(req.RarityMix)
		if stableDigest(params.RarityMixOverride) != stableDigest(normalized) {
			params.RarityMixOverride = normalized
			reasons = append(reasons, "zone_parameter:rarity_mix")
		}
	}
	newState.ZoneParams[zone.ID] = params
	if len(reasons) == 0 {
		return e.Run(ctx)
	}
	if err := validateState(e.region, &newState); err != nil {
		return nil, err
	}
	direct := map[string]bool{zone.ID: true}
	affected := downstreamZones(e.region.Zones, []string{zone.ID})
	return e.applyStateChange(ctx, newState, direct, affected, reasons)
}

func (e *Engine) EditNoCollect(ctx context.Context, req NoCollectEditRequest) (*RunResult, *Error) {
	newState := cloneJSON(e.state)
	old := indexNoCollect(e.state.NoCollect)
	updated := indexNoCollect(newState.NoCollect)
	if !req.Remove {
		if req.ID == "" {
			return nil, schemaError("id", "no-collect id is required")
		}
		if req.Polygon == nil {
			if _, exists := old[req.ID]; !exists {
				return nil, schemaError("polygon", "no-collect polygon is required")
			}
		} else {
			if err := validatePolygon(req.Polygon, "no_collect:"+req.ID, "polygon"); err != nil {
				return nil, err
			}
			for _, p := range req.Polygon {
				if !pointInBounds(p, e.region.Bounds) {
					return nil, geomError("no_collect:"+req.ID, "no-collect vertex lies outside bounds")
				}
			}
			if math.Abs(polygonIntersectionArea(req.Polygon, e.region.Boundary)-polygonArea(req.Polygon)) > 1e-5 {
				return nil, geomError("no_collect:"+req.ID, "no-collect polygon extends outside the region boundary")
			}
			name := req.Name
			if name == "" {
				if existing, ok := old[req.ID]; ok {
					name = existing.Name
				}
			}
			updated[req.ID] = NoCollect{ID: req.ID, Name: name, Polygon: req.Polygon}
		}
	} else {
		delete(updated, req.ID)
	}
	newState.NoCollect = noCollectSlice(updated)
	if err := validateState(e.region, &newState); err != nil {
		return nil, err
	}
	for _, site := range e.region.SampleSites {
		if hit := noCollectHit(newState.NoCollect, site.Point()); hit != nil {
			return nil, sampleError("no_collect:"+hit.ID, site.Point(), "editing no-collect area invalidates a declared sample site")
		}
	}
	direct := make(map[string]bool)
	reasonsByZone := make(map[string][]string)
	newByID := indexNoCollect(newState.NoCollect)
	ids := make([]string, 0, len(old)+len(newByID))
	seen := make(map[string]bool)
	for id := range old {
		ids = append(ids, id)
		seen[id] = true
	}
	for id := range newByID {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		oldEntry, hadOld := old[id]
		newEntry, hasNew := newByID[id]
		if hadOld && hasNew && stableDigest(oldEntry.Polygon) == stableDigest(newEntry.Polygon) {
			continue
		}
		for _, zone := range e.region.Zones {
			touches := (hadOld && polygonOverlaps(zone.Polygon, oldEntry.Polygon)) ||
				(hasNew && polygonOverlaps(zone.Polygon, newEntry.Polygon))
			if touches {
				direct[zone.ID] = true
				reasonsByZone[zone.ID] = append(reasonsByZone[zone.ID], "no_collect:"+id)
			}
		}
	}
	roots := make([]string, 0, len(direct))
	for id := range direct {
		roots = append(roots, id)
	}
	sort.Strings(roots)
	affected := downstreamZones(e.region.Zones, roots)
	reasons := make([]string, 0)
	for _, id := range roots {
		if len(reasonsByZone[id]) == 0 {
			reasonsByZone[id] = []string{"no_collect_boundary"}
		}
	}
	if len(roots) == 0 {
		reasons = append(reasons, "no_collect_boundary")
	}
	return e.applyStateChange(ctx, newState, direct, affected, reasons, reasonsByZone)
}

func (e *Engine) RetryZone(ctx context.Context, zoneID string) (*RunResult, *Error) {
	if findZone(e.region, zoneID) == nil {
		return nil, schemaError("zone_id", "unknown zone %q", zoneID)
	}
	affected := downstreamZones(e.region.Zones, []string{zoneID})
	direct := map[string]bool{zoneID: true}
	return e.applyStateChange(ctx, cloneJSON(e.state), direct, affected, []string{"retry_requested"})
}

func (e *Engine) applyStateChange(
	ctx context.Context,
	newState WorkspaceState,
	direct map[string]bool,
	affected map[string]bool,
	reasons []string,
	perZoneReasons ...map[string][]string,
) (*RunResult, *Error) {
	trial := &Engine{
		region:      e.region,
		state:       normalizeState(newState),
		baseHash:    e.baseHash,
		snapshotID:  e.snapshotID,
		options:     e.options,
		cache:       make(map[string]*ZoneResult, len(e.cache)),
		topology:    e.topology,
		zoneOrder:   e.zoneOrder,
		versionMemo: make(map[string]string),
	}
	trial.stateHash = stableDigest(trial.state)
	for id, result := range e.cache {
		if !affected[id] {
			trial.cache[id] = result
		}
	}
	trial.rebuildVersionMemo()
	recompute := make(map[string]bool, len(affected))
	for id := range affected {
		recompute[id] = true
	}
	for _, id := range e.topology {
		if trial.cache[id] == nil {
			recompute[id] = true
		}
	}
	ids := make([]string, 0, len(recompute))
	for _, id := range e.topology {
		if recompute[id] {
			ids = append(ids, id)
		}
	}
	results, workers, computeErr := trial.computeZones(ctx, ids)
	if computeErr != nil {
		return nil, computeErr
	}
	invalidations := make([]InvalidationEntry, 0, len(e.topology))
	reasonMap := map[string][]string(nil)
	if len(perZoneReasons) > 0 {
		reasonMap = perZoneReasons[0]
	}
	for _, id := range e.topology {
		entry := InvalidationEntry{
			ZoneID:      id,
			Stage:       "zone:reconstruction",
			Invalidated: recompute[id],
			Reused:      !recompute[id],
			Downstream:  recompute[id] && affected[id] && !direct[id],
			CacheState:  CacheReused,
		}
		if recompute[id] {
			entry.CacheState = CacheRecomputed
			zoneReasons := append([]string(nil), reasons...)
			if !affected[id] {
				zoneReasons = []string{"cold_cache"}
			} else if reasonMap != nil && len(reasonMap[id]) > 0 {
				zoneReasons = append([]string(nil), reasonMap[id]...)
			}
			if entry.Downstream {
				zoneReasons = append(zoneReasons, "downstream_dependency")
			}
			if len(zoneReasons) == 0 {
				zoneReasons = []string{"invalidated"}
			}
			entry.Reasons = sortedUniqueStrings(zoneReasons)
		} else {
			entry.Reasons = []string{"unaffected_cache_reused"}
		}
		invalidations = append(invalidations, entry)
	}
	// Persist the editable state before any zone checkpoint. A crash between
	// this write and checkpoint replacement leaves mixed versions that are
	// rejected as a whole on restart.
	stateJSON, _ := json.Marshal(trial.state)
	if e.store != nil {
		if err := e.store.SaveState(e.snapshotID, string(stateJSON), trial.stateHash); err != nil {
			return nil, storeError("save workspace state: %v", err)
		}
	}
	e.state = trial.state
	e.stateHash = trial.stateHash
	e.versionMemo = make(map[string]string)
	e.rebuildVersionMemo()
	recomputed := make([]string, 0, len(ids))
	for _, id := range e.topology {
		result := results[id]
		if result == nil {
			continue
		}
		if err := e.commitResult(result); err != nil {
			for _, pending := range ids {
				delete(e.cache, pending)
			}
			return nil, err
		}
		e.cache[id] = result
		recomputed = append(recomputed, id)
	}
	mode := "incremental"
	if len(recomputed) == 0 {
		mode = "cache"
	} else if len(recomputed) == len(e.topology) {
		mode = "full"
	}
	return e.assembleResultIncremental(mode, recomputed, invalidations, workers)
}

func (e *Engine) assembleResultIncremental(
	mode string,
	recomputed []string,
	invalidations []InvalidationEntry,
	workers WorkerStats,
) (*RunResult, *Error) {
	result, err := e.assembleResult(mode, recomputed, invalidations, workers)
	return result, err
}

func (e *Engine) rebuildVersionMemo() {
	e.versionMemo = make(map[string]string)
}

func (e *Engine) allParamVersions() map[string]string {
	out := make(map[string]string, len(e.topology))
	for _, id := range e.topology {
		out[id] = e.paramVersion(id, map[string]bool{})
	}
	return out
}

func (e *Engine) paramVersion(zoneID string, visiting map[string]bool) string {
	if value, ok := e.versionMemo[zoneID]; ok {
		return value
	}
	if visiting[zoneID] {
		return "cycle"
	}
	zone := findZone(e.region, zoneID)
	if zone == nil {
		return "missing"
	}
	visiting[zoneID] = true
	defer delete(visiting, zoneID)
	deps := make(map[string]string, len(zone.DependsOn))
	for _, dep := range sortedUniqueStrings(zone.DependsOn) {
		deps[dep] = e.paramVersion(dep, visiting)
	}
	noCollect := make([]NoCollect, 0)
	for _, nc := range e.state.NoCollect {
		if polygonOverlaps(zone.Polygon, nc.Polygon) {
			noCollect = append(noCollect, nc)
		}
	}
	sort.Slice(noCollect, func(i, j int) bool { return noCollect[i].ID < noCollect[j].ID })
	payload := map[string]any{
		"base_hash":       e.baseHash,
		"algorithm":       AlgorithmVersion,
		"zone_id":         zoneID,
		"parameters":      e.state.ZoneParams[zoneID],
		"dependencies":    deps,
		"no_collect":      noCollect,
		"sampling_config": e.region.Sampling,
	}
	version := stableDigest(payload)
	e.versionMemo[zoneID] = version
	return version
}

func baseContentHash(region *Region) string {
	copyRegion := cloneJSON(*region)
	copyRegion.Truth = Truth{}
	copyRegion.NoCollect = nil
	for i := range copyRegion.Zones {
		copyRegion.Zones[i].DensityLimit = 0
		copyRegion.Zones[i].RarityMix = nil
	}
	return stableDigest(copyRegion)
}

func normalizeState(state WorkspaceState) WorkspaceState {
	out := cloneJSON(state)
	if out.ZoneParams == nil {
		out.ZoneParams = make(map[string]ZoneParams)
	}
	if out.NoCollect == nil {
		out.NoCollect = []NoCollect{}
	}
	sort.Slice(out.NoCollect, func(i, j int) bool { return out.NoCollect[i].ID < out.NoCollect[j].ID })
	return out
}

func indexNoCollect(values []NoCollect) map[string]NoCollect {
	out := make(map[string]NoCollect, len(values))
	for _, value := range values {
		out[value.ID] = value
	}
	return out
}

func noCollectSlice(values map[string]NoCollect) []NoCollect {
	out := make([]NoCollect, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (e *Engine) CompareFull(ctx context.Context) (*CompareReport, *Error) {
	current, runErr := e.Run(ctx)
	if current == nil {
		return nil, runErr
	}
	fresh, err := NewEngineForTest(e.region, e.options)
	if err != nil {
		return nil, err
	}
	fresh.state = cloneJSON(e.state)
	fresh.stateHash = stableDigest(fresh.state)
	fresh.rebuildVersionMemo()
	full, fullErr := fresh.Run(ctx)
	if full == nil {
		return nil, fullErr
	}
	report := &CompareReport{
		Equal:            true,
		IncrementalOrder: append([]string(nil), current.CommitOrder...),
		FullOrder:        append([]string(nil), full.CommitOrder...),
	}
	report.CheckedItems++
	if !equalStrings(current.CommitOrder, full.CommitOrder) {
		report.Equal = false
		report.FirstDataItem = "commit_order"
		return report, divergenceError(report)
	}
	for _, id := range current.CommitOrder {
		left := findZoneResult(current, id)
		right := findZoneResult(full, id)
		if left == nil || right == nil {
			report.Equal = false
			report.FirstZoneID = id
			report.FirstDataItem = "zone_result"
			return report, divergenceError(report)
		}
		checks := []struct {
			name string
			ok   bool
		}{
			{"sample_indices", equalIntSlices(left.SampleSelection.Indices, right.SampleSelection.Indices)},
			{"sample_site_ids", stableDigest(left.SampleSelection.SiteIDs) == stableDigest(right.SampleSelection.SiteIDs)},
			{"sample_strata", stableDigest(left.SampleSelection.Strata) == stableDigest(right.SampleSelection.Strata)},
			{"density", left.Estimate.Density == right.Estimate.Density},
			{"raw_density", left.Estimate.RawDensity == right.Estimate.RawDensity},
			{"density_limit", left.Estimate.DensityLimit == right.Estimate.DensityLimit},
			{"standard_error", left.Estimate.StandardError == right.Estimate.StandardError},
			{"strata", stableDigest(left.Estimate.Strata) == stableDigest(right.Estimate.Strata)},
			{"rarity_mix", stableDigest(left.Estimate.RarityMix) == stableDigest(right.Estimate.RarityMix)},
			{"observed_rarity", stableDigest(left.Estimate.ObservedRarity) == stableDigest(right.Estimate.ObservedRarity)},
			{"rarity_residual", left.Estimate.RarityResidual == right.Estimate.RarityResidual},
			{"effective_area", left.Estimate.EffectiveArea == right.Estimate.EffectiveArea},
			{"sample_count", left.Estimate.SampleCount == right.Estimate.SampleCount},
			{"point_count", left.PointCount == right.PointCount},
			{"point_digest", left.PointDigest == right.PointDigest},
			{"points", stableDigest(left.Points) == stableDigest(right.Points)},
			{"heat_digest", left.HeatDigest == right.HeatDigest},
			{"heat_layer", stableDigest(left.HeatLayer) == stableDigest(right.HeatLayer)},
			{"metrics", stableDigest(left.Metrics) == stableDigest(right.Metrics)},
			{"param_version", left.ParamVersion == right.ParamVersion},
		}
		for _, check := range checks {
			report.CheckedItems++
			if !check.ok {
				report.Equal = false
				report.FirstZoneID = id
				report.FirstDataItem = check.name
				return report, divergenceError(report)
			}
		}
	}
	report.CheckedItems++
	if len(current.Heatmap.Values) != len(full.Heatmap.Values) {
		report.Equal = false
		report.FirstDataItem = "heatmap_shape"
		return report, divergenceError(report)
	}
	for i := range current.Heatmap.Values {
		if current.Heatmap.Values[i] != full.Heatmap.Values[i] {
			report.Equal = false
			report.FirstDataItem = "heatmap_values"
			return report, divergenceError(report)
		}
	}
	report.CheckedItems++
	if stableDigest(current.Bias) != stableDigest(full.Bias) {
		report.Equal = false
		report.FirstDataItem = "bias_report"
		return report, divergenceError(report)
	}
	return report, nil
}

func divergenceError(report *CompareReport) *Error {
	return &Error{
		Code:     CodeDiverge,
		ZoneID:   report.FirstZoneID,
		DataItem: report.FirstDataItem,
		Message:  "incremental result diverges from a cold full rebuild",
	}
}

func findZoneResult(result *RunResult, id string) *ZoneResult {
	for _, zone := range result.Zones {
		if zone.ZoneID == id {
			return zone
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
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

func equalIntSlices(a, b []int) bool {
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

func (e *Engine) View(run *RunResult) View {
	view := View{
		RegionID:      e.region.RegionID,
		DisplayName:   e.region.DisplayName,
		Bounds:        e.region.Bounds,
		Boundary:      e.region.Boundary,
		NoCollect:     cloneJSON(e.state.NoCollect),
		Terrain:       e.region.TerrainRaster,
		TerrainTypes:  e.region.TerrainTypes,
		SampleSites:   e.region.SampleSites,
		Bias:          run.Bias,
		Invalidations: run.Invalidations,
		CommitOrder:   run.CommitOrder,
		Worker:        run.Worker,
		Dependency:    run.Dependency,
		Snapshot:      run.Snapshot,
		Mode:          run.Mode,
		Heatmap:       run.Heatmap,
		Points:        run.Points,
	}
	cacheStates := make(map[string]CacheState)
	for _, entry := range run.Invalidations {
		cacheStates[entry.ZoneID] = entry.CacheState
	}
	for _, zone := range run.Zones {
		params := e.state.ZoneParams[zone.ZoneID]
		model := buildZoneModel(e.region, e.state.NoCollect, findZone(e.region, zone.ZoneID))
		viewZone := ZoneView{
			ID:               zone.ZoneID,
			Name:             findZone(e.region, zone.ZoneID).Name,
			Polygon:          findZone(e.region, zone.ZoneID).Polygon,
			TerrainTypes:     findZone(e.region, zone.ZoneID).TerrainTypes,
			DensityLimit:     params.DensityLimit,
			RarityMix:        cloneJSON(zone.Estimate.RarityMix),
			RarityOverride:   cloneJSON(params.RarityMixOverride),
			DependsOn:        append([]string(nil), findZone(e.region, zone.ZoneID).DependsOn...),
			EstimatedDensity: zone.Estimate.Density,
			StandardError:    zone.Estimate.StandardError,
			SampleCount:      zone.Estimate.SampleCount,
			EffectiveArea:    model.EffectiveArea,
			PointCount:       zone.PointCount,
			PointDigest:      zone.PointDigest,
			HeatDigest:       zone.HeatDigest,
			CacheState:       cacheStates[zone.ZoneID],
			ParamVersion:     zone.ParamVersion,
			Generation:       zone.Generation,
		}
		view.Zones = append(view.Zones, viewZone)
	}
	return view
}

func EncodeResult(result any) ([]byte, *Error) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, ioError("encode report: %v", err)
	}
	return data, nil
}

func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var coded *Error
	if errors.As(err, &coded) {
		return coded
	}
	return ioError("%v", err)
}

func ErrorJSON(err error) []byte {
	coded := AsError(err)
	data, _ := json.Marshal(map[string]any{"ok": false, "error": coded})
	return data
}
