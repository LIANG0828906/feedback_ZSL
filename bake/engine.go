package bake

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	"terrainbake/geometry"
	"terrainbake/model"
)

// Engine owns a project revision, terrain index, cache, and optional store.
type Engine struct {
	mu      sync.Mutex
	project *model.Project
	terrain *geometry.Terrain
	cache   *Cache
	store   UnitStore
}

// NewEngine builds an engine. The project is treated as the current revision.
func NewEngine(p *model.Project, store UnitStore) (*Engine, error) {
	if p == nil {
		return nil, model.NewError(model.CodeValue, "engine requires a project")
	}
	terrain, err := geometry.New(p)
	if err != nil {
		return nil, err
	}
	return &Engine{
		project: p,
		terrain: terrain,
		cache:   NewCache(),
		store:   store,
	}, nil
}

// Project returns the current project revision.
func (e *Engine) Project() *model.Project {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.project
}

// Terrain returns the current immutable terrain index.
func (e *Engine) Terrain() *geometry.Terrain {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.terrain
}

// Cache exposes the engine cache for diagnostics.
func (e *Engine) Cache() *Cache {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cache
}

// LoadPersisted loads version- and content-matched units. A mismatch invalidates
// the entire loaded snapshot and returns E_SNAP.
func (e *Engine) LoadPersisted(ctx context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.store == nil {
		return 0, nil
	}
	units, err := e.store.LoadUnits(ctx)
	if err != nil {
		return 0, err
	}
	refs := make([]UnitRef, 0, len(units))
	for _, unit := range units {
		refs = append(refs, UnitRef{ChunkID: unit.ChunkID, Time: unit.Time})
	}
	sortRefs(refs)
	byRef := make(map[string]*BakedUnit, len(units))
	for _, unit := range units {
		byRef[UnitID(unit.ChunkID, unit.Time)] = unit
	}
	previousKeys := make(map[string]string)
	for _, ref := range refs {
		unit := byRef[UnitID(ref.ChunkID, ref.Time)]
		if _, err := e.validateCachedLocked(unit, previousKeys[ref.ChunkID]); err != nil {
			e.cache = NewCache()
			_ = e.store.DeleteUnits(ctx, refs)
			if resetter, ok := e.store.(interface{ Reset(context.Context) error }); ok {
				_ = resetter.Reset(ctx)
			}
			return 0, model.NewError(model.CodeSnap,
				"persisted snapshot is incompatible with current project: %v", err).WithChunk(unit.ChunkID)
		}
		previousKeys[ref.ChunkID] = unit.CacheKey
	}
	for _, unit := range units {
		e.cache.Put(unit)
	}
	return len(units), nil
}

// BakeFull clears the working cache and bakes every unit from scratch.
func (e *Engine) BakeFull(ctx context.Context, opts ScheduleOptions) (*BakeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if resetter, ok := e.store.(interface{ Reset(context.Context) error }); ok {
		if err := resetter.Reset(ctx); err != nil {
			return nil, err
		}
	}
	e.cache = NewCache()
	return e.bakeCurrentLocked(ctx, opts, nil, nil)
}

// BakeFullOrder bakes from an empty cache using an explicit chunk processing
// order. Commit sequence remains canonical, proving result independence.
func (e *Engine) BakeFullOrder(ctx context.Context, chunkOrder []string, opts ScheduleOptions) (*BakeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	known := make(map[string]struct{}, len(e.project.Chunks))
	for _, c := range e.project.Chunks {
		known[c.ID] = struct{}{}
	}
	if len(chunkOrder) != len(known) {
		return nil, model.NewError(model.CodeValue, "explicit chunk order must be a complete permutation").WithField("order")
	}
	for _, id := range chunkOrder {
		if _, ok := known[id]; !ok {
			return nil, model.NewError(model.CodeValue, "explicit chunk order contains unknown chunk").WithChunk(id)
		}
		delete(known, id)
	}
	if len(known) != 0 {
		return nil, model.NewError(model.CodeValue, "explicit chunk order is incomplete").WithField("order")
	}
	if resetter, ok := e.store.(interface{ Reset(context.Context) error }); ok {
		if err := resetter.Reset(ctx); err != nil {
			return nil, err
		}
	}
	e.cache = NewCache()
	return e.bakeCurrentLocked(ctx, opts, nil, chunkOrder)
}

// BakeIncremental bakes missing or invalid units and reuses unaffected cache.
func (e *Engine) BakeIncremental(ctx context.Context, opts ScheduleOptions) (*BakeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bakeCurrentLocked(ctx, opts, nil, nil)
}

func (e *Engine) bakeCurrentLocked(ctx context.Context, opts ScheduleOptions, explicitInvalid map[string]Invalidation, explicitOrder []string) (*BakeResult, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	sequence := BuildCommitSequence(e.project)
	result := &BakeResult{
		Units:          make(map[string]*BakedUnit, len(sequence)),
		CommitSequence: sequence,
		Stats: ScheduleStats{
			MaxParallelism: opts.MaxParallelism, VertexBufferCap: opts.VertexBufferCap, QueueBudget: opts.QueueBudget,
		},
	}
	sunByTime := make(map[int]model.SunSample, len(e.project.Sun))
	for _, sun := range e.project.Sun {
		sunByTime[sun.Time] = sun
	}
	times := make([]int, 0, len(e.project.Sun))
	for t := range sunByTime {
		times = append(times, t)
	}
	sort.Ints(times)
	chunks := make([]string, 0, len(e.project.Chunks))
	for _, c := range e.project.Chunks {
		chunks = append(chunks, c.ID)
	}
	sort.Strings(chunks)
	if len(explicitOrder) > 0 {
		chunks = append([]string(nil), explicitOrder...)
	}
	previousKeys := make(map[string]string, len(chunks))

	for _, timeValue := range times {
		sun := sunByTime[timeValue]
		var tasks []unitTask
		for _, chunkID := range chunks {
			cached, has := e.cache.Get(chunkID, timeValue)
			if has {
				_, invalidExplicitly := explicitInvalid[UnitID(chunkID, timeValue)]
				if !invalidExplicitly {
					if _, err := e.validateCachedLocked(cached, previousKeys[chunkID]); err == nil {
						result.Units[UnitID(chunkID, timeValue)] = cached
						result.Reused = append(result.Reused, UnitRef{ChunkID: chunkID, Time: timeValue})
						continue
					}
				}
				e.cache.Delete(chunkID, timeValue)
			}
			tasks = append(tasks, unitTask{
				ref:         UnitRef{ChunkID: chunkID, Time: timeValue},
				sun:         sun,
				previousKey: previousKeys[chunkID],
			})
		}
		units, waveStats, err := runBakeWave(ctx, e.terrain, tasks, opts)
		if err != nil {
			return result, err
		}
		mergeStats(&result.Stats, waveStats)
		waveRefs := make([]UnitRef, 0, len(units))
		for _, unit := range units {
			waveRefs = append(waveRefs, UnitRef{ChunkID: unit.ChunkID, Time: unit.Time})
		}
		sortRefs(waveRefs)
		for _, ref := range waveRefs {
			unit := units[UnitID(ref.ChunkID, ref.Time)]
			if e.store != nil {
				if err := e.store.SaveUnit(ctx, unit); err != nil {
					return result, err
				}
			}
			e.cache.Put(unit)
			result.Units[UnitID(ref.ChunkID, ref.Time)] = unit
			result.Rebaked = append(result.Rebaked, ref)
		}
		for _, chunkID := range chunks {
			unit, ok := result.Units[UnitID(chunkID, timeValue)]
			if !ok {
				return result, model.NewError(model.CodeValue, "unit missing after bake wave").
					WithChunk(chunkID).WithTime(timeValue)
			}
			previousKeys[chunkID] = unit.CacheKey
		}
	}
	sortRefs(result.Rebaked)
	sortRefs(result.Reused)
	return result, nil
}

func (e *Engine) validateCachedLocked(unit *BakedUnit, previousKey string) (*BakedUnit, error) {
	if unit == nil {
		return nil, model.NewError(model.CodeValue, "nil cached unit")
	}
	chunk, ok := e.terrain.Chunk(unit.ChunkID)
	if !ok {
		return nil, model.NewError(model.CodeValue, "cached unit references unknown chunk").WithChunk(unit.ChunkID)
	}
	sun, ok := findSun(e.project, unit.Time)
	if !ok {
		return nil, model.NewError(model.CodeValue, "cached unit references unknown sun time").WithTime(unit.Time)
	}
	p := e.project.Params
	if unit.ContentHash != model.ChunkContentHash(*chunk) {
		return nil, model.NewError(model.CodeValue, "chunk content hash does not match").WithChunk(unit.ChunkID)
	}
	if unit.HeightHash != model.ChunkHeightHash(*chunk) {
		return nil, model.NewError(model.CodeValue, "chunk height hash does not match").WithChunk(unit.ChunkID)
	}
	if unit.SunHash != model.SunHash(sun) || unit.SkyHash != model.SkySetHash(e.project.Sky) {
		return nil, model.NewError(model.CodeValue, "sun or sky hash does not match").WithChunk(unit.ChunkID)
	}
	if unit.Dependencies.OccluderHash != model.OccluderSetHash(e.project.Occluders) {
		return nil, model.NewError(model.CodeValue, "occluder geometry hash does not match").WithChunk(unit.ChunkID)
	}
	if unit.ParameterVersion != p.ParameterVersion || unit.AlgorithmVersion != p.AlgorithmVersion {
		return nil, model.NewError(model.CodeValue, "parameter or algorithm version does not match").WithChunk(unit.ChunkID)
	}
	if unit.Dependencies.TemporalKey != previousKey {
		return nil, model.NewError(model.CodeValue, "temporal cache upstream does not match").WithChunk(unit.ChunkID)
	}
	expected := e.rebuildCacheKey(unit, previousKey)
	if expected != unit.CacheKey {
		return nil, model.NewError(model.CodeValue, "upstream cache key does not match").WithChunk(unit.ChunkID)
	}
	return unit, nil
}

func (e *Engine) rebuildCacheKey(unit *BakedUnit, previousKey string) string {
	p := e.project.Params
	neighbors := make([]string, 0, len(unit.Dependencies.SourceChunks))
	upstream := make([]string, 0, len(unit.Dependencies.SourceChunks)+1)
	if previousKey != "" {
		upstream = append(upstream, previousKey)
	}
	upstream = append(upstream, "occluders:"+model.OccluderSetHash(e.project.Occluders))
	for _, id := range unit.Dependencies.SourceChunks {
		if id == unit.ChunkID {
			continue
		}
		chunk, ok := e.terrain.Chunk(id)
		if !ok {
			continue
		}
		h := model.ChunkHeightHash(*chunk)
		neighbors = append(neighbors, h)
		upstream = append(upstream, TerrainCacheKey(id, h, p))
	}
	sort.Strings(upstream)
	return model.CombineCacheKey(unit.ChunkID, unit.HeightHash, neighbors, unit.Time,
		unit.SunHash, p.ParameterVersion, p.AlgorithmVersion, upstream)
}

// InvalidateHeight removes only cached units whose ray paths used chunkID, plus
// the changed chunk itself and their later logical times.
func (e *Engine) InvalidateHeight(ctx context.Context, chunkID string) ([]Invalidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.terrain.Chunk(chunkID); !ok {
		return nil, model.NewError(model.CodeValue, "unknown chunk %q", chunkID).WithChunk(chunkID)
	}
	return e.invalidateHeightLocked(ctx, chunkID)
}

func (e *Engine) invalidateHeightLocked(ctx context.Context, chunkID string) ([]Invalidation, error) {
	units := e.cache.Snapshot()
	marked := make(map[string]Invalidation)
	for _, unit := range units {
		reason := ""
		dep := ""
		if unit.ChunkID == chunkID {
			reason, dep = "height_changed", "height:"+chunkID
		} else {
			for _, source := range unit.Dependencies.SourceChunks {
				if source == chunkID {
					reason, dep = "height_changed", "height:"+chunkID
					break
				}
			}
		}
		if reason != "" {
			marked[UnitID(unit.ChunkID, unit.Time)] = Invalidation{
				ChunkID: unit.ChunkID, Time: unit.Time, Reason: reason, Dependency: dep,
			}
		}
	}
	for _, unit := range units {
		if _, ok := marked[UnitID(unit.ChunkID, unit.Time)]; ok {
			for _, downstream := range units {
				if downstream.ChunkID != unit.ChunkID || downstream.Time <= unit.Time {
					continue
				}
				key := UnitID(downstream.ChunkID, downstream.Time)
				if _, exists := marked[key]; exists {
					continue
				}
				marked[key] = Invalidation{
					ChunkID: downstream.ChunkID, Time: downstream.Time,
					Reason: "upstream", Dependency: "cache:" + UnitID(unit.ChunkID, unit.Time),
				}
			}
		}
	}
	return e.applyInvalidationLocked(ctx, marked)
}

// SetSun replaces one logical-time solar sample and invalidates that time and
// subsequent times. Earlier times remain reusable.
func (e *Engine) SetSun(ctx context.Context, replacement model.SunSample) ([]Invalidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	index := -1
	for i := range e.project.Sun {
		if e.project.Sun[i].Time == replacement.Time {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, model.NewError(model.CodeValue, "unknown sun time %d", replacement.Time).WithTime(replacement.Time)
	}
	if !finite(replacement.AzimuthDeg) || !finite(replacement.ElevationDeg) ||
		replacement.ElevationDeg < 0 || replacement.ElevationDeg > 90 {
		return nil, model.NewError(model.CodeValue, "replacement solar direction is invalid").
			WithField("sun.azimuth_deg").WithTime(replacement.Time)
	}
	maxPhysical := float64(e.project.Params.MaxChannelFixed) / float64(e.project.Params.FixedScale)
	for ch, value := range replacement.Radiance {
		if !finite(value) || value < 0 || value > maxPhysical {
			return nil, model.NewError(model.CodeValue, "replacement solar radiance is outside [0,%g]", maxPhysical).
				WithField("sun.radiance").WithChannel(channelName(ch)).WithTime(replacement.Time)
		}
	}
	check := append([]model.SunSample(nil), e.project.Sun...)
	check[index] = replacement
	for i := 1; i < len(check); i++ {
		if check[i].AzimuthDeg+1e-9 < check[i-1].AzimuthDeg {
			return nil, model.NewError(model.CodeValue, "solar azimuth must remain monotonic").
				WithField(fmt.Sprintf("sun[%d].azimuth_deg", i)).WithTime(check[i].Time)
		}
	}
	marked := make(map[string]Invalidation)
	for _, unit := range e.cache.Snapshot() {
		if unit.Time < replacement.Time {
			continue
		}
		reason, dep := "upstream", "cache:"+UnitID(unit.ChunkID, replacement.Time)
		if unit.Time == replacement.Time {
			reason, dep = "sun_changed", fmt.Sprintf("sun:%d", replacement.Time)
		}
		marked[UnitID(unit.ChunkID, unit.Time)] = Invalidation{
			ChunkID: unit.ChunkID, Time: unit.Time, Reason: reason, Dependency: dep,
		}
	}
	invalidations, err := e.applyInvalidationLocked(ctx, marked)
	if err != nil {
		return nil, err
	}
	e.project.Sun[index] = replacement
	return invalidations, nil
}

func (e *Engine) applyInvalidationLocked(ctx context.Context, marked map[string]Invalidation) ([]Invalidation, error) {
	refs := make([]UnitRef, 0, len(marked))
	out := make([]Invalidation, 0, len(marked))
	for _, inv := range marked {
		refs = append(refs, UnitRef{ChunkID: inv.ChunkID, Time: inv.Time})
		out = append(out, inv)
	}
	sortRefs(refs)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time < out[j].Time
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	if len(refs) == 0 {
		return out, nil
	}
	if e.store != nil {
		if err := e.store.DeleteUnits(ctx, refs); err != nil {
			return nil, err
		}
	}
	e.cache.DeleteBatch(refs)
	return out, nil
}

// RaiseHeight edits one height sample and returns precise invalidations. The
// snapshot rows are removed before the project revision changes.
func (e *Engine) RaiseHeight(ctx context.Context, chunkID string, vertex model.VertexPoint, delta float64) ([]Invalidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	chunk, ok := e.terrain.Chunk(chunkID)
	if !ok {
		return nil, model.NewError(model.CodeValue, "unknown chunk %q", chunkID).WithChunk(chunkID)
	}
	if vertex.X < 0 || vertex.X >= chunk.ResX || vertex.Z < 0 || vertex.Z >= chunk.ResZ {
		return nil, model.NewError(model.CodeValue, "height edit vertex is outside chunk resolution").
			WithChunk(chunkID).WithPoint(vertex)
	}
	targetWorld := chunk.VertexAt(vertex.X, vertex.Z)
	targets := e.sharedHeightTargets(targetWorld)
	if !finite(delta) {
		return nil, model.NewError(model.CodeValue, "height delta must be finite").
			WithChunk(chunkID).WithPoint(vertex)
	}
	for _, target := range targets {
		if !finite(target.chunk.Heights[target.index] + delta) {
			return nil, model.NewError(model.CodeValue, "height edit produces a non-finite sample").
				WithChunk(target.chunkID).WithPoint(vertex)
		}
	}
	if delta == 0 {
		return nil, nil
	}
	affectedChunks := make([]string, 0, len(targets))
	for _, target := range targets {
		affectedChunks = append(affectedChunks, target.chunkID)
	}
	merged := make(map[string]Invalidation)
	for _, affected := range affectedChunks {
		invalidations, err := e.invalidateHeightLocked(ctx, affected)
		if err != nil {
			return nil, err
		}
		for _, inv := range invalidations {
			merged[UnitID(inv.ChunkID, inv.Time)] = inv
		}
	}
	for _, target := range targets {
		target.chunk.Heights[target.index] += delta
	}
	terrain, err := geometry.New(e.project)
	if err != nil {
		return nil, err
	}
	e.terrain = terrain
	invalidations := make([]Invalidation, 0, len(merged))
	for _, inv := range merged {
		invalidations = append(invalidations, inv)
	}
	sort.Slice(invalidations, func(i, j int) bool {
		if invalidations[i].Time != invalidations[j].Time {
			return invalidations[i].Time < invalidations[j].Time
		}
		return invalidations[i].ChunkID < invalidations[j].ChunkID
	})
	return invalidations, nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

type heightTarget struct {
	chunkID string
	chunk   *model.Chunk
	index   int
}

func (e *Engine) sharedHeightTargets(world model.Vec3) []heightTarget {
	var targets []heightTarget
	for i := range e.project.Chunks {
		chunk := &e.project.Chunks[i]
		for iz := 0; iz < chunk.ResZ; iz++ {
			for ix := 0; ix < chunk.ResX; ix++ {
				pos := chunk.VertexAt(ix, iz)
				if math.Abs(pos.X-world.X) <= 1e-9 && math.Abs(pos.Z-world.Z) <= 1e-9 {
					targets = append(targets, heightTarget{
						chunkID: chunk.ID,
						chunk:   chunk,
						index:   chunk.VertexIndex(ix, iz),
					})
				}
			}
		}
	}
	return targets
}

func findSun(p *model.Project, timeValue int) (model.SunSample, bool) {
	for _, sun := range p.Sun {
		if sun.Time == timeValue {
			return sun, true
		}
	}
	return model.SunSample{}, false
}

// FixedScaleErrorBound returns the conservative per-channel truncation bound
// for the declared accumulation stages. It is monotone non-increasing with
// increasing scale.
func FixedScaleErrorBound(scale int64) float64 {
	if scale <= 0 {
		return 1
	}
	return 7.0 / float64(scale)
}
