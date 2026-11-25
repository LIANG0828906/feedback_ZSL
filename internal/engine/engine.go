package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"

	"atelier/internal/apperr"
	"atelier/internal/geometry"
	"atelier/internal/hashutil"
	"atelier/internal/model"
	"atelier/internal/validate"
)

type Engine struct {
	mu           sync.RWMutex
	model        model.DensityModel
	zoneOrder    []model.Zone
	zoneByID     map[string]model.Zone
	observations map[string][]model.Observation
	states       map[string]*model.ZoneState
	heat         *model.Heatmap
	revision     uint64
	pool         *workerPool
	terrainHash  string
	configHash   string
	scenarioHash string
	checkpoint   CheckpointSink
	sessionID    string
	lastInvalid  *InvalidationReport
	recovery     *RecoveryReport
	lastMetrics  RunMetrics
}

func New(scenario model.Scenario) (*Engine, error) {
	if err := validate.Scenario(&scenario); err != nil {
		return nil, err
	}
	return newFromModel(model.ModelFromScenario(scenario), true)
}

func newFromModel(m model.DensityModel, validated bool) (*Engine, error) {
	m = cloneModel(m)
	if !validated {
		if err := validateDensityModel(m); err != nil {
			return nil, err
		}
	}
	zones := append([]model.Zone(nil), m.Region.Zones...)
	sort.SliceStable(zones, func(i, j int) bool {
		if zones[i].Ordinal == zones[j].Ordinal {
			return zones[i].ID < zones[j].ID
		}
		return zones[i].Ordinal < zones[j].Ordinal
	})
	zoneByID := make(map[string]model.Zone, len(zones))
	for _, z := range zones {
		zoneByID[z.ID] = z
	}
	obs := map[string][]model.Observation{}
	for _, o := range m.Observations {
		obs[o.ZoneID] = append(obs[o.ZoneID], o)
	}
	for zoneID := range obs {
		sort.Slice(obs[zoneID], func(i, j int) bool {
			a, b := obs[zoneID][i], obs[zoneID][j]
			if a.GridRow != b.GridRow {
				return a.GridRow < b.GridRow
			}
			return a.GridCol < b.GridCol
		})
	}
	e := &Engine{
		model:        m,
		zoneOrder:    zones,
		zoneByID:     zoneByID,
		observations: obs,
		states:       map[string]*model.ZoneState{},
		revision:     0,
		terrainHash:  hashutil.JSON(m.Region.Terrain),
		configHash:   hashutil.JSON(m.Config),
	}
	e.scenarioHash = hashutil.JSON(struct {
		Region       model.Region
		Config       model.SamplingConfig
		Observations []model.Observation
	}{m.Region, m.Config, m.Observations})
	return e, nil
}

func validateDensityModel(m model.DensityModel) error {
	if m.Region.ID == "" || len(m.Region.Zones) == 0 {
		return apperr.Schema("density model is missing region or zones")
	}
	if m.Config.SampleGridStep <= 0 || m.Config.SamplesPerZone <= 0 ||
		m.Config.HeatmapCellSize <= 0 || m.Config.HeatmapTileSize <= 0 ||
		m.Config.MaxParallelism <= 0 || m.Config.PerWorkerBufferSize <= 0 ||
		m.Config.QuantizationStep <= 0 {
		return apperr.Schema("density model config is incomplete")
	}
	if len(m.Observations) == 0 {
		return apperr.Schema("density model is missing observations")
	}
	return nil
}

func (e *Engine) SetCheckpointSink(sink CheckpointSink) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.checkpoint = sink
}

func (e *Engine) SetRecovery(report RecoveryReport) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recovery = &report
}

func (e *Engine) ExpectedParamsVersions() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]string, len(e.zoneOrder))
	for _, zone := range e.zoneOrder {
		out[zone.ID] = e.paramsVersion(zone)
	}
	return out
}

func (e *Engine) ZoneOrder() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return zoneOrderIDs(e.zoneOrder)
}

func (e *Engine) ScenarioHash() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.scenarioHash
}

func (e *Engine) ResumeFromCheckpoint(ctx context.Context, zones []model.ZoneState, report RecoveryReport) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	order := zoneOrderIDs(e.zoneOrder)
	invs := map[string]ZoneInvalidation{}
	for i, zoneID := range order {
		zone := e.zoneByID[zoneID]
		if i < len(zones) {
			state := zones[i]
			if state.ZoneID != zoneID || state.ParamsVersion != e.paramsVersion(zone) {
				return Snapshot{}, apperr.Schema("checkpoint zone %s does not match current inputs", zoneID)
			}
			copyState := state
			e.states[zoneID] = &copyState
		} else {
			invs[zoneID] = ZoneInvalidation{
				ZoneID: zoneID, Resample: true, Reestimate: true,
				Reconstruct: true, Heatmap: true,
				Reason: "zone not present in recovered checkpoint",
			}
		}
	}
	e.recovery = &report
	return e.runLocked(ctx, ModeIncremental, invs, nil)
}

func (e *Engine) FullRebuild(ctx context.Context) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runLocked(ctx, ModeFull, nil, nil)
}

func (e *Engine) UpdateZoneParams(ctx context.Context, zoneID string, params model.ZoneParams) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	old, ok := e.zoneByID[zoneID]
	if !ok {
		return Snapshot{}, apperr.Schema("unknown zone %q", zoneID)
	}
	if params.DensityScale <= 0 || params.DensityScale > 10 {
		return Snapshot{}, apperr.Schema("zone %s densityScale must be in (0,10]", zoneID)
	}
	if math.IsNaN(params.RarityShift) || math.Abs(params.RarityShift) > 0.5 {
		return Snapshot{}, apperr.Schema("zone %s rarityShift must be in [-0.5,0.5]", zoneID)
	}
	densityChanged := math.Abs(params.DensityScale-old.Params.DensityScale) > 1e-12
	rarityChanged := math.Abs(params.RarityShift-old.Params.RarityShift) > 1e-12
	if !densityChanged && !rarityChanged {
		return e.snapshotLocked(), nil
	}
	e.replaceZoneParams(zoneID, params)
	inv := ZoneInvalidation{
		ZoneID:      zoneID,
		Reestimate:  true,
		Reconstruct: true,
		Heatmap:     densityChanged,
		Reason:      "zone parameters changed",
	}
	return e.runLocked(ctx, ModeIncremental, map[string]ZoneInvalidation{zoneID: inv}, nil)
}

func (e *Engine) MoveExclusion(ctx context.Context, exclusionID string, dx, dy float64) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	idx := -1
	for i := range e.model.Region.Exclusions {
		if e.model.Region.Exclusions[i].ID == exclusionID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Snapshot{}, apperr.Schema("unknown exclusion %q", exclusionID)
	}
	oldPoly := e.model.Region.Exclusions[idx].Polygon
	newPoly := translatePolygon(oldPoly, dx, dy)
	if err := e.validateMovedExclusion(newPoly); err != nil {
		return Snapshot{}, err
	}
	e.model.Region.Exclusions[idx].Polygon = newPoly
	affected := e.zonesAffectedByPolygonChange(oldPoly, newPoly)
	invs := map[string]ZoneInvalidation{}
	for _, zoneID := range affected {
		invs[zoneID] = ZoneInvalidation{
			ZoneID:      zoneID,
			Resample:    true,
			Reestimate:  true,
			Reconstruct: true,
			Heatmap:     true,
			Reason:      fmt.Sprintf("exclusion %s moved into or out of zone", exclusionID),
		}
	}
	return e.runLocked(ctx, ModeIncremental, invs, nil)
}

func (e *Engine) UpdateZoneBoundary(ctx context.Context, zoneID string, polygon model.Polygon) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.zoneByID[zoneID]; !ok {
		return Snapshot{}, apperr.Schema("unknown zone %q", zoneID)
	}
	if err := e.validateZonePolygon(zoneID, polygon); err != nil {
		return Snapshot{}, err
	}
	e.replaceZonePolygon(zoneID, polygon)
	inv := ZoneInvalidation{
		ZoneID: zoneID, Resample: true, Reestimate: true,
		Reconstruct: true, Heatmap: true, Reason: "zone boundary changed",
	}
	return e.runLocked(ctx, ModeIncremental, map[string]ZoneInvalidation{zoneID: inv}, nil)
}

func (e *Engine) RetryZone(ctx context.Context, zoneID string) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.zoneByID[zoneID]; !ok {
		return Snapshot{}, apperr.Schema("unknown zone %q", zoneID)
	}
	inv := ZoneInvalidation{
		ZoneID: zoneID, Resample: true, Reestimate: true,
		Reconstruct: true, Heatmap: true, Reason: "manual per-zone retry",
	}
	return e.runLocked(ctx, ModeIncremental, map[string]ZoneInvalidation{zoneID: inv}, nil)
}

func (e *Engine) runLocked(ctx context.Context, mode RunMode, invs map[string]ZoneInvalidation, prepared map[string]*model.ZoneState) (Snapshot, error) {
	e.revision++
	full := mode == ModeFull
	if full {
		e.states = map[string]*model.ZoneState{}
		e.heat = nil
		invs = map[string]ZoneInvalidation{}
		for _, zone := range e.zoneOrder {
			invs[zone.ID] = ZoneInvalidation{
				ZoneID: zone.ID, Resample: true, Reestimate: true,
				Reconstruct: true, Heatmap: true, Reason: "cache empty; full rebuild",
			}
		}
	}
	if invs == nil {
		invs = map[string]ZoneInvalidation{}
	}
	for id, inv := range invs {
		if inv.Resample {
			inv.Reestimate = true
		}
		if inv.Reestimate {
			inv.Reconstruct = true
		}
		invs[id] = inv
	}
	sessionID := ""
	if e.checkpoint != nil {
		id, err := e.checkpoint.BeginSession(ctx, e.scenarioHash, zoneOrderIDs(e.zoneOrder))
		if err != nil {
			return Snapshot{}, err
		}
		sessionID = id
		e.sessionID = id
	}
	finish := func(status string) {
		if e.checkpoint != nil && sessionID != "" {
			_ = e.checkpoint.FinishSession(context.Background(), sessionID, status)
		}
	}
	e.pool = newWorkerPool(e.model.Config.MaxParallelism, e.model.Config.PerWorkerBufferSize)
	defer e.pool.Close()
	zoneTaskCount := 0
	for _, zone := range e.zoneOrder {
		if _, ok := invs[zone.ID]; ok {
			zoneTaskCount++
		}
	}
	states, err := e.computeZones(ctx, invs, prepared)
	if err != nil {
		finish("cancelled")
		return Snapshot{}, err
	}
	for id, state := range states {
		e.states[id] = state
	}
	affectedHeat := map[string]bool{}
	for _, inv := range invs {
		if inv.Heatmap {
			affectedHeat[inv.ZoneID] = true
		}
	}
	heat, tileStats, err := e.buildHeatmap(ctx, affectedHeat)
	if err != nil {
		finish("cancelled")
		return Snapshot{}, err
	}
	e.heat = &heat
	e.finalizeHeatSummaries(&heat)
	e.lastInvalid = e.makeInvalidation(invs, tileStats, full)
	if e.checkpoint != nil && sessionID != "" {
		for _, state := range e.sortedStates() {
			if err := e.checkpoint.SaveZone(ctx, sessionID, state); err != nil {
				finish("failed")
				return Snapshot{}, fmt.Errorf("%s: save checkpoint: %w", apperr.CodeIO, err)
			}
		}
	}
	finish("completed")
	lastMetrics := e.pool.Metrics()
	lastMetrics.ZoneTasks = zoneTaskCount
	lastMetrics.HeatmapTileTasks = len(tileStats)
	e.lastMetrics = lastMetrics
	snap := e.snapshotLocked()
	return snap, nil
}

func (e *Engine) computeZones(ctx context.Context, invs map[string]ZoneInvalidation, prepared map[string]*model.ZoneState) (map[string]*model.ZoneState, error) {
	var ids []string
	for _, zone := range e.zoneOrder {
		if _, ok := invs[zone.ID]; ok {
			ids = append(ids, zone.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return e.zoneByID[ids[i]].Ordinal < e.zoneByID[ids[j]].Ordinal
	})
	results := make([]*model.ZoneState, len(ids))
	errs := make([]error, len(ids))
	err := e.pool.Run(ctx, len(ids), func(index int) {
		zoneID := ids[index]
		zone := e.zoneByID[zoneID]
		var prev *model.ZoneState
		if prepared != nil {
			prev = prepared[zoneID]
		}
		if prev == nil {
			prev = e.states[zoneID]
		}
		state, err := e.buildZoneState(zone, invs[zoneID], prev)
		results[index] = state
		errs[index] = err
	})
	if err != nil {
		return nil, err
	}
	out := map[string]*model.ZoneState{}
	for i, id := range ids {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out[id] = results[i]
	}
	return out, nil
}

func (e *Engine) buildZoneState(zone model.Zone, inv ZoneInvalidation, prev *model.ZoneState) (*model.ZoneState, error) {
	state := &model.ZoneState{
		ZoneID:        zone.ID,
		Ordinal:       zone.Ordinal,
		ParamsVersion: e.paramsVersion(zone),
		Revision:      e.revision,
	}
	state.SamplingKey = e.samplingKey(zone)
	state.EstimateKey = hashutil.JSON(struct {
		SamplingKey   string
		Params        model.ZoneParams
		ParamsVersion string
		Quantization  float64
	}{state.SamplingKey, zone.Params, state.ParamsVersion, e.model.Config.QuantizationStep})
	state.PointsKey = hashutil.JSON(struct {
		EstimateKey string
		Seed        int64
		Params      model.ZoneParams
	}{state.EstimateKey, e.model.Seed, zone.Params})
	state.CacheKey = hashutil.JSON([]string{state.SamplingKey, state.EstimateKey, state.PointsKey})

	if !inv.Resample && prev != nil {
		state.Samples = prev.Samples
	} else {
		state.Samples = e.selectSamples(zone)
	}
	if !inv.Reestimate && prev != nil {
		state.Estimate = prev.Estimate
	} else {
		state.Estimate = e.estimateZone(zone, state.Samples)
	}
	if !inv.Reconstruct && prev != nil {
		state.Points = prev.Points
	} else {
		state.Points = e.reconstructZone(zone, state.Estimate)
	}
	state.ContentHash = hashZoneState(*state, "")
	return state, nil
}

func (e *Engine) paramsVersion(zone model.Zone) string {
	return hashutil.JSON(struct {
		Zone        model.Zone
		Exclusions  []model.Exclusion
		ConfigHash  string
		TerrainHash string
	}{zone, e.zoneRelevantExclusions(zone), e.configHash, e.terrainHash})
}

func (e *Engine) samplingKey(zone model.Zone) string {
	var obs []model.Observation
	for _, o := range e.observations[zone.ID] {
		p := model.Point{X: o.X, Y: o.Y}
		if geometry.StrictlyInside(p, zone.Polygon) && !e.excluded(p) {
			obs = append(obs, o)
		}
	}
	return hashutil.JSON(struct {
		ZoneID     string
		Polygon    model.Polygon
		Ordinal    int
		Exclusions []model.Exclusion
		Config     model.SamplingConfig
		Obs        []model.Observation
		Seed       int64
	}{
		ZoneID: zone.ID, Polygon: zone.Polygon, Ordinal: zone.Ordinal,
		Exclusions: e.zoneRelevantExclusions(zone), Config: e.model.Config,
		Obs: obs, Seed: e.model.Seed,
	})
}

func (e *Engine) finalizeHeatSummaries(heat *model.Heatmap) {
	for _, state := range e.states {
		values := make([]float64, 0)
		for i, zoneID := range heat.ZoneIDs {
			if zoneID == state.ZoneID {
				values = append(values, heat.Values[i])
			}
		}
		state.HeatSummary = hashutil.JSON(values)
		state.ContentHash = hashZoneState(*state, state.HeatSummary)
	}
}

func hashZoneState(state model.ZoneState, heatSummary string) string {
	return hashutil.JSON(struct {
		SamplingKey string
		EstimateKey string
		PointsKey   string
		Samples     []model.SamplePoint
		Estimate    model.DensityEstimate
		Points      []model.ResourcePoint
		HeatSummary string
	}{
		state.SamplingKey, state.EstimateKey, state.PointsKey,
		state.Samples, state.Estimate, state.Points, heatSummary,
	})
}

func (e *Engine) makeInvalidation(invs map[string]ZoneInvalidation, stats []heatTileStats, full bool) *InvalidationReport {
	report := &InvalidationReport{
		Explanation: "Only affected zone components and heatmap cells are invalidated. Mixed tiles copy unaffected pixels from the previous raster.",
	}
	for _, zone := range e.zoneOrder {
		if inv, ok := invs[zone.ID]; ok {
			report.AffectedZones = append(report.AffectedZones, inv)
			report.AffectedNodes = append(report.AffectedNodes,
				"zone:"+zone.ID+":params",
				"zone:"+zone.ID+":estimate",
				"zone:"+zone.ID+":points",
			)
			if inv.Resample {
				report.AffectedNodes = append(report.AffectedNodes, "zone:"+zone.ID+":sampling")
			}
		} else {
			report.ReusedZoneCount++
		}
	}
	for _, tile := range stats {
		var zones []string
		for zoneID := range tile.byZone {
			zones = append(zones, zoneID)
		}
		sort.Strings(zones)
		tileAffected := false
		for _, zoneID := range zones {
			zs := tile.byZone[zoneID]
			state := "reused"
			if zs.recomputed > 0 && zs.reused > 0 {
				state = "partial"
			} else if zs.recomputed > 0 {
				state = "invalidated"
			}
			reason := "inputs unchanged; cached pixels reused"
			if zs.recomputed > 0 {
				tileAffected = true
				if inv, ok := invs[zoneID]; ok {
					reason = inv.Reason
				} else if full {
					reason = "cache empty; full rebuild"
				} else {
					reason = "pixel depends on an invalidated zone"
				}
			}
			report.Entries = append(report.Entries, InvalidationEntry{
				ZoneID: zoneID, TileX: tile.tileX, TileY: tile.tileY,
				ReuseState: state, Reason: reason,
				ReusedPixels: zs.reused, RecomputedPixels: zs.recomputed,
			})
			report.ReusedPixels += zs.reused
			report.RecomputedPixels += zs.recomputed
			if zs.recomputed > 0 {
				report.AffectedNodes = append(report.AffectedNodes,
					fmt.Sprintf("heat:tile:%d:%d", tile.tileX, tile.tileY))
			}
		}
		if tileAffected {
			report.AffectedTileCount++
		} else {
			report.ReusedTileCount++
		}
	}
	sort.Strings(report.AffectedNodes)
	report.AffectedNodes = uniqueStrings(report.AffectedNodes)
	return report
}

func (e *Engine) snapshotLocked() Snapshot {
	states := e.sortedStates()
	points := canonicalPoints(states)
	heat := model.Heatmap{}
	if e.heat != nil {
		heat = *e.heat
		heat.Values = append([]float64(nil), e.heat.Values...)
		heat.ZoneIDs = append([]string(nil), e.heat.ZoneIDs...)
		heat.TileHashes = append([]string(nil), e.heat.TileHashes...)
	}
	zones := append([]model.Zone(nil), e.zoneOrder...)
	exclusions := append([]model.Exclusion(nil), e.model.Region.Exclusions...)
	terrain := e.model.Region.Terrain
	terrain.Types = append([]string(nil), terrain.Types...)
	snap := Snapshot{
		RegionID:         e.model.RegionID,
		Seed:             e.model.Seed,
		AlgorithmVersion: model.AlgorithmVersion,
		Revision:         e.revision,
		Bounds:           e.model.Region.Boundary.Bounds(),
		Config:           e.model.Config,
		Boundary:         e.model.Region.Boundary,
		Zones:            zones,
		Exclusions:       exclusions,
		Terrain:          terrain,
		ZoneStates:       states,
		Points:           points,
		Heatmap:          heat,
		Report:           e.buildReport(states, heat),
		Graph:            e.buildGraph(),
		Invalidation:     cloneInvalidation(e.lastInvalid),
		CacheHitSequence: cacheTrace(states),
		Tolerances:       DefaultTolerances(),
	}
	snap.Metrics = e.poolMetrics()
	if e.recovery != nil {
		r := *e.recovery
		snap.Recovery = &r
	}
	if e.checkpoint != nil && e.sessionID != "" {
		snap.Checkpoint = &CheckpointStatus{
			SessionID: e.sessionID, CommittedZones: len(states),
			Order: zoneOrderIDs(e.zoneOrder), AlgorithmVersion: model.AlgorithmVersion,
		}
	}
	return snap
}

func (e *Engine) Snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snapshotLocked()
}

func (e *Engine) CompareWithFull(ctx context.Context) (ComparisonResult, Snapshot, error) {
	e.mu.RLock()
	current := e.snapshotLocked()
	m := cloneModel(e.model)
	e.mu.RUnlock()
	fullEngine, err := newFromModel(m, false)
	if err != nil {
		return ComparisonResult{}, Snapshot{}, err
	}
	full, err := fullEngine.FullRebuild(ctx)
	if err != nil {
		return ComparisonResult{}, Snapshot{}, err
	}
	result, err := CompareSnapshots(current, full)
	return result, full, err
}

func (e *Engine) poolMetrics() RunMetrics {
	if e.lastMetrics.MaxParallelism == 0 {
		return RunMetrics{
			MaxParallelism:      e.model.Config.MaxParallelism,
			PerWorkerBufferSize: e.model.Config.PerWorkerBufferSize,
		}
	}
	return e.lastMetrics
}

func (e *Engine) sortedStates() []model.ZoneState {
	out := make([]model.ZoneState, 0, len(e.states))
	for _, zone := range e.zoneOrder {
		if state, ok := e.states[zone.ID]; ok {
			out = append(out, *state)
		}
	}
	return out
}

func (e *Engine) replaceZoneParams(zoneID string, params model.ZoneParams) {
	zone := e.zoneByID[zoneID]
	zone.Params = params
	e.zoneByID[zoneID] = zone
	for i := range e.zoneOrder {
		if e.zoneOrder[i].ID == zoneID {
			e.zoneOrder[i].Params = params
		}
	}
	for i := range e.model.Region.Zones {
		if e.model.Region.Zones[i].ID == zoneID {
			e.model.Region.Zones[i].Params = params
		}
	}
}

func (e *Engine) replaceZonePolygon(zoneID string, polygon model.Polygon) {
	zone := e.zoneByID[zoneID]
	zone.Polygon = polygon
	e.zoneByID[zoneID] = zone
	for i := range e.zoneOrder {
		if e.zoneOrder[i].ID == zoneID {
			e.zoneOrder[i].Polygon = polygon
		}
	}
	for i := range e.model.Region.Zones {
		if e.model.Region.Zones[i].ID == zoneID {
			e.model.Region.Zones[i].Polygon = polygon
		}
	}
}

func (e *Engine) validateMovedExclusion(poly model.Polygon) error {
	if len(poly.Vertices) < 4 || !sameKeptPoint(poly.Vertices[0], poly.Vertices[len(poly.Vertices)-1]) {
		return apperr.Geom("", 0, "moved exclusion polygon is not closed")
	}
	if idx, bad := geometry.FirstSelfIntersection(poly); bad {
		return apperr.Geom("", idx+1, "moved exclusion self-intersects at vertex %d", idx+1)
	}
	vi, ok := geometry.PolygonInside(poly, e.model.Region.Boundary)
	if !ok {
		return apperr.Geom("", vi+1, "moved exclusion vertex %d falls outside the region", vi+1)
	}
	return nil
}

func (e *Engine) validateZonePolygon(zoneID string, poly model.Polygon) error {
	if len(poly.Vertices) < 4 || !sameKeptPoint(poly.Vertices[0], poly.Vertices[len(poly.Vertices)-1]) {
		return apperr.Geom(zoneID, 0, "zone %s polygon is not closed", zoneID)
	}
	if idx, bad := geometry.FirstSelfIntersection(poly); bad {
		return apperr.Geom(zoneID, idx+1, "zone %s self-intersects at vertex %d", zoneID, idx+1)
	}
	if poly.Area() <= 1e-6 {
		return apperr.Geom(zoneID, 0, "zone %s polygon has zero area", zoneID)
	}
	if vi, ok := geometry.PolygonInside(poly, e.model.Region.Boundary); !ok {
		return apperr.Geom(zoneID, vi+1, "zone %s vertex %d falls outside the region", zoneID, vi+1)
	}
	for _, other := range e.zoneOrder {
		if other.ID == zoneID {
			continue
		}
		if geometry.PolygonsOverlap(poly, other.Polygon) {
			return apperr.Geom(zoneID, 0, "zone %s boundary overlaps zone %s", zoneID, other.ID)
		}
	}
	return nil
}

func (e *Engine) zonesAffectedByPolygonChange(oldPoly, newPoly model.Polygon) []string {
	var out []string
	for _, zone := range e.zoneOrder {
		if geometry.PolygonsOverlap(zone.Polygon, oldPoly) ||
			geometry.PolygonsOverlap(zone.Polygon, newPoly) ||
			polygonInsidePolygon(oldPoly, zone.Polygon) ||
			polygonInsidePolygon(newPoly, zone.Polygon) {
			out = append(out, zone.ID)
		}
	}
	sort.Strings(out)
	return out
}

func polygonInsidePolygon(inner, outer model.Polygon) bool {
	_, ok := geometry.PolygonInside(inner, outer)
	return ok
}

func translatePolygon(poly model.Polygon, dx, dy float64) model.Polygon {
	out := model.Polygon{Vertices: make([]model.Point, len(poly.Vertices))}
	for i, v := range poly.Vertices {
		out.Vertices[i] = model.Point{X: v.X + dx, Y: v.Y + dy}
	}
	return out
}

func sameKeptPoint(a, b model.Point) bool {
	return math.Abs(a.X-b.X) < 1e-7 && math.Abs(a.Y-b.Y) < 1e-7
}

func cloneInvalidation(in *InvalidationReport) *InvalidationReport {
	if in == nil {
		return nil
	}
	out := *in
	out.AffectedZones = append([]ZoneInvalidation(nil), in.AffectedZones...)
	out.Entries = append([]InvalidationEntry(nil), in.Entries...)
	out.AffectedNodes = append([]string(nil), in.AffectedNodes...)
	return &out
}

func cloneModel(m model.DensityModel) model.DensityModel {
	b, _ := json.Marshal(m)
	var out model.DensityModel
	_ = json.Unmarshal(b, &out)
	return out
}

func uniqueStrings(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func zoneOrderIDs(zones []model.Zone) []string {
	out := make([]string, 0, len(zones))
	for _, zone := range zones {
		out = append(out, zone.ID)
	}
	return out
}
