package fog

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type RenderMode string

const (
	RenderPreview RenderMode = "preview"
	RenderExact   RenderMode = "exact"
)

type RenderOptions struct {
	Mode             RenderMode
	CameraIndex      int
	ForceRecompute   bool
	NoPersist        bool
	TileLimit        int
	FailTile         string
	FailMediaID      string
	ShadowBudget     int
	MaxParallelTiles int
}

type RenderDecision struct {
	Mode             RenderMode `json:"mode"`
	StepsConfigured  int        `json:"steps_configured"`
	StepsActual      int        `json:"steps_actual"`
	ShadowConfigured int        `json:"shadow_configured"`
	ShadowActual     int        `json:"shadow_actual"`
	ShadowBudget     int        `json:"shadow_budget"`
	LatencyLimitMS   int64      `json:"latency_limit_ms"`
	ElapsedMS        float64    `json:"elapsed_ms"`
	LatencyMet       bool       `json:"latency_met"`
	Reason           string     `json:"reason"`
}

type RenderStats struct {
	Tiles               int          `json:"tiles"`
	RenderedTiles       int          `json:"rendered_tiles"`
	CacheHits           int          `json:"cache_hits"`
	CacheMisses         int          `json:"cache_misses"`
	Rays                int          `json:"rays"`
	Steps               int          `json:"steps"`
	ShadowSamples       int          `json:"shadow_samples"`
	MaxParallelObserved int          `json:"max_parallel_observed"`
	RayBufferPeak       int          `json:"ray_buffer_peak"`
	ShadowBudget        BudgetStats  `json:"shadow_budget"`
	QueueEvents         []QueueEvent `json:"queue_events,omitempty"`
	CacheHitTiles       []string     `json:"cache_hit_tiles,omitempty"`
	CommittedTiles      []string     `json:"committed_tiles,omitempty"`
}

type Frame struct {
	Mode                 string         `json:"mode"`
	CameraIndex          int            `json:"camera_index"`
	CameraTime           int64          `json:"camera_time"`
	Width                int            `json:"width"`
	Height               int            `json:"height"`
	MediaSnapshotVersion string         `json:"media_snapshot_version"`
	CameraVersion        string         `json:"camera_version"`
	RecipeVersion        string         `json:"recipe_version"`
	AlgorithmVersion     string         `json:"algorithm_version"`
	StructureVersion     string         `json:"structure_version"`
	Tiles                []TileResult   `json:"tiles"`
	Pixels               []PixelValue   `json:"pixels"`
	Stats                RenderStats    `json:"stats"`
	Decision             RenderDecision `json:"decision"`
	Faults               FaultList      `json:"faults,omitempty"`
	CacheInvalidations   FaultList      `json:"cache_invalidations,omitempty"`
	CommitSequence       []string       `json:"commit_sequence,omitempty"`
	StartedAt            time.Time      `json:"started_at"`
}

func (f *Frame) FirstFault() *Fault {
	if f == nil || len(f.Faults) == 0 {
		return nil
	}
	return f.Faults[0]
}

func (f *Frame) PixelAt(x, y int) (PixelValue, bool) {
	i := y*f.Width + x
	if i < 0 || i >= len(f.Pixels) {
		return PixelValue{}, false
	}
	p := f.Pixels[i]
	return p, p.X == x && p.Y == y
}

type Engine struct {
	Scene *Scene
	Store *CheckpointStore

	mu         sync.Mutex
	graph      *DependencyGraph
	lastFrame  *Frame
	renderLock sync.Mutex
}

func NewEngine(scene *Scene, store *CheckpointStore) *Engine {
	return &Engine{Scene: scene, Store: store}
}

func (e *Engine) DependencyGraph() *DependencyGraph {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.graph
}

func (e *Engine) LastFrame() *Frame {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastFrame
}

func (e *Engine) SnapshotVersion() string {
	return e.Scene.ContentHash(e.Scene.Recipe.CoefficientScale)
}

type tileSpec struct {
	ID string
	X  int
	Y  int
	W  int
	H  int
}

func buildTileSpecs(scene *Scene, cam Camera, limit int) []tileSpec {
	tw, th := scene.Recipe.TileWidth, scene.Recipe.TileHeight
	var out []tileSpec
	rows := (scene.Height + th - 1) / th
	cols := (scene.Width + tw - 1) / tw
	for ty := 0; ty < rows; ty++ {
		for tx := 0; tx < cols; tx++ {
			w := minInt(tw, scene.Width-tx*tw)
			h := minInt(th, scene.Height-ty*th)
			out = append(out, tileSpec{
				ID: fmt.Sprintf("tile-r%02d-c%02d", ty, tx),
				X:  tx * tw, Y: ty * th, W: w, H: h,
			})
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
	}
	_ = cam
	return out
}

func (e *Engine) Render(ctx context.Context, opts RenderOptions) (*Frame, error) {
	e.renderLock.Lock()
	defer e.renderLock.Unlock()
	if opts.Mode == "" {
		opts.Mode = RenderExact
	}
	if opts.CameraIndex < 0 {
		opts.CameraIndex = 0
	}
	scene := e.Scene.Clone()
	if f := scene.Validate(); f != nil {
		return nil, f
	}
	if opts.CameraIndex >= len(scene.Cameras) {
		return nil, &Fault{Code: EPara, Path: "camera_index", Actual: opts.CameraIndex, Expected: fmt.Sprintf("0..%d", len(scene.Cameras)-1), Message: "camera index out of range"}
	}
	cam := scene.Cameras[opts.CameraIndex]
	start := time.Now()
	profile, steps, shadows := renderProfile(scene, opts.Mode)
	shadowCap := scene.Recipe.ShadowBudget
	if opts.ShadowBudget > 0 {
		shadowCap = opts.ShadowBudget
	}
	maxParallel := scene.Recipe.MaxParallelTiles
	if opts.MaxParallelTiles > 0 {
		maxParallel = opts.MaxParallelTiles
	}
	budget := newShadowBudget(shadowCap)
	slots := newBlockingCounter(maxParallel)
	specs := buildTileSpecs(scene, cam, opts.TileLimit)
	frame := &Frame{
		Mode: string(opts.Mode), CameraIndex: opts.CameraIndex, CameraTime: cam.LogicalTime,
		Width: scene.Width, Height: scene.Height,
		MediaSnapshotVersion: scene.ContentHash(scene.Recipe.CoefficientScale),
		CameraVersion:        scene.CameraHash(),
		RecipeVersion:        scene.RecipeHash(),
		AlgorithmVersion:     scene.Recipe.AlgorithmVersion,
		StructureVersion:     scene.StructureHash(),
		StartedAt:            start,
		Decision: RenderDecision{
			Mode: opts.Mode, StepsConfigured: steps, ShadowConfigured: shadows,
			ShadowBudget: shadowCap, LatencyLimitMS: scene.Recipe.PreviewLatencyMS,
			Reason: "full-profile",
		},
	}

	cached := map[string]TileResult{}
	if e.Store != nil && opts.Mode == RenderExact && !opts.ForceRecompute {
		loaded, invalid, _ := e.Store.LoadFor(scene, profile)
		frame.CacheInvalidations = append(frame.CacheInvalidations, invalid...)
		for _, sp := range specs {
			if t, ok := loaded[sp.ID]; ok && tileMatchesSpec(t, sp) {
				cached[sp.ID] = t
			}
		}
	}

	type result struct {
		idx  int
		tile TileResult
		err  *Fault
	}
	results := make([]result, len(specs))
	var wg sync.WaitGroup
	for i := range specs {
		sp := specs[i]
		if t, ok := cached[sp.ID]; ok {
			results[i] = result{idx: i, tile: t}
			continue
		}
		wg.Add(1)
		go func(idx int, sp tileSpec) {
			defer wg.Done()
			if _, err := slots.acquire(ctx, sp.ID); err != nil {
				results[idx] = result{idx: idx, err: &Fault{Code: ECanceled, TileID: sp.ID, Path: "scheduler.tile_slot", Message: "render canceled while queued", Cause: err.Error()}}
				return
			}
			defer slots.release()
			if opts.FailTile != "" && opts.FailTile == sp.ID {
				media := opts.FailMediaID
				if media == "" {
					media = "unknown"
				}
				results[idx] = result{idx: idx, err: &Fault{Code: EBlock, TileID: sp.ID, MediaID: media, Path: "tile." + sp.ID, Actual: "injected-failure", Message: "injected single-block failure"}}
				return
			}
			tile, fault := renderTile(ctx, scene, cam, sp, profile, steps, shadows, budget)
			if fault != nil {
				results[idx] = result{idx: idx, err: fault}
				return
			}
			results[idx] = result{idx: idx, tile: tile}
		}(i, sp)
	}
	wg.Wait()

	graph := NewDependencyGraph()
	fresh := make([]TileResult, 0, len(specs))
	var tileErrors FaultList
	frame.Tiles = make([]TileResult, 0, len(specs))
	for i := range results {
		r := results[i]
		if r.err != nil {
			tileErrors = append(tileErrors, r.err)
			continue
		}
		t := r.tile
		_, wasCached := cached[t.ID]
		updateKeys := !wasCached
		if fault := attachTileDependencies(scene, cam, &t, graph, updateKeys, profile); fault != nil {
			tileErrors = append(tileErrors, fault)
			continue
		}
		if !wasCached {
			fresh = append(fresh, t)
		} else {
			frame.Stats.CacheHits++
			frame.Stats.CacheHitTiles = append(frame.Stats.CacheHitTiles, t.ID)
		}
		frame.Tiles = append(frame.Tiles, t)
		frame.Stats.Steps += t.Steps
		frame.Stats.ShadowSamples += t.ShadowSamples
		frame.Stats.Rays += len(t.Rays)
	}
	sort.Slice(frame.Tiles, func(i, j int) bool {
		if frame.Tiles[i].Y != frame.Tiles[j].Y {
			return frame.Tiles[i].Y < frame.Tiles[j].Y
		}
		return frame.Tiles[i].X < frame.Tiles[j].X
	})
	frame.Stats.RenderedTiles = len(fresh)
	frame.Stats.CacheMisses = len(specs) - frame.Stats.CacheHits - len(tileErrors)
	frame.Stats.Tiles = len(frame.Tiles)
	sort.Strings(frame.Stats.CacheHitTiles)
	frame.Stats.MaxParallelObserved = int(atomic.LoadInt32(&slots.peak))
	for _, t := range fresh {
		if t.RayBufferPeak > frame.Stats.RayBufferPeak {
			frame.Stats.RayBufferPeak = t.RayBufferPeak
		}
	}
	frame.Stats.ShadowBudget = budget.stats()
	frame.Stats.QueueEvents = append(frame.Stats.QueueEvents, slots.eventsSnapshot()...)
	frame.Stats.QueueEvents = append(frame.Stats.QueueEvents, budget.queueEvents()...)
	sort.Slice(frame.Stats.QueueEvents, func(i, j int) bool {
		a, b := frame.Stats.QueueEvents[i], frame.Stats.QueueEvents[j]
		if a.TileID != b.TileID {
			return a.TileID < b.TileID
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.Sequence < b.Sequence
	})

	if e.Store != nil && opts.Mode == RenderExact && !opts.NoPersist {
		committed, err := e.Store.CommitTilesStable(fresh)
		if err != nil {
			return nil, &Fault{Code: ECheckpt, Path: "checkpoint.commit", Message: "stable tile commit failed", Cause: err.Error()}
		}
		for id := range committed {
			frame.Stats.CommittedTiles = append(frame.Stats.CommittedTiles, id)
		}
		sort.Strings(frame.Stats.CommittedTiles)
		frame.CommitSequence = e.Store.CommitSequence()
	} else {
		for _, t := range fresh {
			frame.CommitSequence = append(frame.CommitSequence, t.ID)
		}
		sort.Strings(frame.CommitSequence)
	}
	frame.Faults = tileErrors
	frame.Pixels = flattenPixels(frame)
	maxRaySteps := 0
	maxRayShadows := 0
	for _, t := range frame.Tiles {
		for _, rr := range t.Rays {
			if rr.Steps > maxRaySteps {
				maxRaySteps = rr.Steps
			}
			if rr.ShadowSamples > maxRayShadows {
				maxRayShadows = rr.ShadowSamples
			}
		}
	}
	frame.Decision.StepsActual = maxRaySteps
	frame.Decision.ShadowActual = maxRayShadows
	frame.Decision.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
	frame.Decision.LatencyMet = frame.Decision.ElapsedMS <= float64(scene.Recipe.PreviewLatencyMS)
	if opts.Mode == RenderPreview {
		if frame.Decision.LatencyMet {
			frame.Decision.Reason = "preview-profile: steps and shadow samples reduced; latency inside declared limit"
		} else {
			frame.Decision.Reason = "preview-profile: reduced quality, declared latency exceeded"
		}
	} else {
		frame.Decision.Reason = "exact-profile: full steps and shadow cap"
	}
	e.mu.Lock()
	e.graph = graph
	e.lastFrame = frame
	e.mu.Unlock()
	if len(tileErrors) > 0 {
		return frame, tileErrors
	}
	return frame, nil
}

func renderProfile(scene *Scene, mode RenderMode) (string, int, int) {
	if mode == RenderPreview {
		return fmt.Sprintf("preview:s%d:h%d", scene.Recipe.PreviewSteps, scene.Recipe.PreviewShadow),
			scene.Recipe.PreviewSteps, scene.Recipe.PreviewShadow
	}
	return fmt.Sprintf("exact:s%d:h%d", scene.Recipe.Steps, scene.Recipe.ShadowSamples),
		scene.Recipe.Steps, scene.Recipe.ShadowSamples
}

func tileMatchesSpec(t TileResult, sp tileSpec) bool {
	return t.X == sp.X && t.Y == sp.Y && t.W == sp.W && t.H == sp.H
}

func renderTile(ctx context.Context, scene *Scene, cam Camera, sp tileSpec, profile string, steps, shadows int, budget *shadowBudget) (TileResult, *Fault) {
	t := TileResult{
		ID: sp.ID, X: sp.X, Y: sp.Y, W: sp.W, H: sp.H,
		Profile: profile, CameraVersion: scene.CameraHash(), RecipeVersion: scene.RecipeHash(),
		AlgorithmVersion: scene.Recipe.AlgorithmVersion, StructureVersion: scene.StructureHash(),
		MediaHashes: map[string]string{},
	}
	bufferLimit := scene.Recipe.RayBufferPerWorker
	if bufferLimit < 1 {
		bufferLimit = 1
	}
	type coord struct{ x, y int }
	batch := make([]coord, 0, bufferLimit)
	process := func() *Fault {
		for _, c := range batch {
			ray := CameraRay(cam, c.x, c.y, scene.Width, scene.Height)
			in := IntersectRay(scene, ray)
			rr, fault := IntegrateRay(ctx, scene, ray, in, IntegrateOptions{
				Steps: steps, ShadowSamples: shadows, TileID: sp.ID, Profile: profile, Budget: budget,
			})
			if fault != nil {
				return fault
			}
			t.Rays = append(t.Rays, rr)
		}
		batch = batch[:0]
		return nil
	}
	for y := sp.Y; y < sp.Y+sp.H; y++ {
		for x := sp.X; x < sp.X+sp.W; x++ {
			batch = append(batch, coord{x, y})
			if len(batch) == bufferLimit {
				if f := process(); f != nil {
					return t, f
				}
			}
		}
	}
	if len(batch) > 0 {
		if f := process(); f != nil {
			return t, f
		}
	}
	t.RayBufferPeak = minInt(bufferLimit, sp.W*sp.H)
	t.Steps = 0
	t.ShadowSamples = 0
	for _, rr := range t.Rays {
		t.Steps += rr.Steps
		t.ShadowSamples += rr.ShadowSamples
	}
	t.Pixels = make([]PixelValue, 0, len(t.Rays))
	for _, rr := range t.Rays {
		t.Pixels = append(t.Pixels, PixelValue{
			X: rr.X, Y: rr.Y, Radiance: rr.Radiance, Transmittance: rr.Transmittance,
			RayDigest: rr.Digest, CacheKey: rr.CacheKey,
		})
	}
	return t, nil
}

func attachTileDependencies(scene *Scene, cam Camera, t *TileResult, graph *DependencyGraph, updateKeys bool, profile string) *Fault {
	tileNode := TileNodeID(t.ID)
	graph.AddNode(tileNode, "tile", t.ID)
	graph.AddNode(CameraSource(t.CameraVersion), "camera", t.CameraVersion)
	graph.AddNode(RecipeSource(t.RecipeVersion), "recipe", t.RecipeVersion)
	mediaHashes := map[string]string{}
	dependencyHashes := map[string]string{}
	pixelIndex := map[[2]int]int{}
	for i := range t.Pixels {
		pixelIndex[[2]int{t.Pixels[i].X, t.Pixels[i].Y}] = i
	}
	for ri := range t.Rays {
		rr := &t.Rays[ri]
		ray := CameraRay(cam, rr.X, rr.Y, scene.Width, scene.Height)
		pixelNode := PixelNodeID(rr.RayID)
		graph.AddNode(pixelNode, "pixel", rr.RayID)
		graph.AddEdge(pixelNode, tileNode)
		rayMedia := map[string]bool{}
		for si := range rr.Segments {
			sr := &rr.Segments[si]
			segNode := SegmentNodeID(rr.RayID, si)
			graph.AddNode(segNode, "segment", fmt.Sprintf("%s#%d", rr.RayID, si))
			graph.AddEdge(segNode, pixelNode)
			graph.AddEdge(CameraSource(t.CameraVersion), segNode)
			graph.AddEdge(RecipeSource(t.RecipeVersion), segNode)
			for _, mediaID := range sr.Segment.ActiveMedia {
				_, m := scene.MediaByID(mediaID)
				if m == nil {
					continue
				}
				rayMedia[mediaID] = true
				coeffNode := CoefficientSource(mediaID)
				graph.AddNode(coeffNode, "coefficient", mediaID)
				graph.AddEdge(coeffNode, segNode)
				if _, ok := dependencyHashes[coeffNode]; !ok {
					dependencyHashes[coeffNode] = CoefficientContentHash(m, scene.Recipe.CoefficientScale)
				}
				for _, voxel := range VoxelsAlongSegment(*m, ray, sr.Segment, scene.Recipe.FixedScale) {
					dn := DensitySource(mediaID, voxel)
					graph.AddNode(dn, "density", fmt.Sprintf("%s[%d]", mediaID, voxel))
					graph.AddEdge(dn, segNode)
					if _, ok := dependencyHashes[dn]; !ok {
						dependencyHashes[dn] = DensityContentHash(m, voxel)
					}
				}
			}
			if sr.ShadowSamples > 0 {
				for mediaID, voxels := range shadowDependencies(scene, ray, sr) {
					_, m := scene.MediaByID(mediaID)
					if m == nil {
						continue
					}
					rayMedia[mediaID] = true
					coeffNode := CoefficientSource(mediaID)
					graph.AddNode(coeffNode, "coefficient", mediaID)
					graph.AddEdge(coeffNode, segNode)
					if _, ok := dependencyHashes[coeffNode]; !ok {
						dependencyHashes[coeffNode] = CoefficientContentHash(m, scene.Recipe.CoefficientScale)
					}
					for _, voxel := range voxels {
						dn := DensitySource(mediaID, voxel)
						graph.AddNode(dn, "density", fmt.Sprintf("%s[%d]", mediaID, voxel))
						graph.AddEdge(dn, segNode)
						if _, ok := dependencyHashes[dn]; !ok {
							dependencyHashes[dn] = DensityContentHash(m, voxel)
						}
					}
				}
			}
		}
		ids := make([]string, 0, len(rayMedia))
		for id := range rayMedia {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			mediaHashes[id] = MediaDependencyHash(id, dependencyHashes)
		}
		if updateKeys {
			depIDs := make([]string, 0, len(dependencyHashes))
			for id := range dependencyHashes {
				depIDs = append(depIDs, id)
			}
			sort.Strings(depIDs)
			parts := make([]string, 0, len(depIDs)*2)
			for _, id := range depIDs {
				parts = append(parts, id, dependencyHashes[id])
			}
			rayContent := hashStrings(parts...)
			upstream := rr.IntersectionDigest
			for si := range rr.Segments {
				key := ComputeCacheKey(rayContent, fmt.Sprintf("%s#%d", rr.RayID, si), rayCacheParamVersion(scene), upstream)
				rr.Segments[si].CacheKey = key
				upstream = key
			}
			rr.CacheKey = ComputeCacheKey(rayContent, rr.RayID, rayCacheParamVersion(scene), upstream)
			if idx, ok := pixelIndex[[2]int{rr.X, rr.Y}]; ok {
				t.Pixels[idx].CacheKey = rr.CacheKey
			}
		}
	}
	if updateKeys {
		t.MediaHashes = mediaHashes
		t.DependencyHashes = dependencyHashes
		t.Finalize()
	} else {
		// Keep the persisted cache key/content hash untouched for reused
		// blocks, but refresh the digest field only if it was absent.
		if t.SegmentDigest == "" {
			t.SegmentDigest = hashStrings(tileRayDigests(t.Rays)...)
		}
	}
	_ = profile
	return nil
}

func shadowDependencies(scene *Scene, ray Ray, sr *SegmentResult) map[string][]int {
	out := map[string][]int{}
	if sr.ShadowSamples <= 0 {
		return out
	}
	scale := scene.Recipe.FixedScale
	mid := FixedToFloat(sr.Segment.T0, scale) + sr.Segment.Length(scale)*0.5
	p := ray.PointAt(mid)
	dir := scene.Recipe.LightDirection.Normalize()
	maxT := scene.Recipe.ShadowDistance
	for mi := range scene.Media {
		m := &scene.Media[mi]
		t0, t1, ok := intersectAABB(p, dir, m.Bounds)
		if !ok {
			continue
		}
		t0 = math.Max(t0, 0)
		t1 = math.Min(t1, maxT)
		if t1 <= t0 {
			continue
		}
		q0, _ := Quantize(t0, scale)
		q1, _ := Quantize(t1, scale)
		if q1 <= q0 {
			continue
		}
		shadowSeg := Segment{T0: q0, T1: q1}
		out[m.ID] = VoxelsAlongSegment(*m, Ray{Origin: p, Direction: dir}, shadowSeg, scale)
	}
	return out
}

func flattenPixels(frame *Frame) []PixelValue {
	out := make([]PixelValue, 0, frame.Width*frame.Height)
	for y := 0; y < frame.Height; y++ {
		for x := 0; x < frame.Width; x++ {
			found := false
			for _, t := range frame.Tiles {
				if x < t.X || x >= t.X+t.W || y < t.Y || y >= t.Y+t.H {
					continue
				}
				idx := (y-t.Y)*t.W + (x - t.X)
				if idx >= 0 && idx < len(t.Pixels) {
					p := t.Pixels[idx]
					if p.X == x && p.Y == y {
						out = append(out, p)
						found = true
						break
					}
				}
			}
			if !found {
				out = append(out, PixelValue{X: x, Y: y, Missing: true})
			}
		}
	}
	return out
}

func (e *Engine) InvalidateDensity(mediaID string, voxel int) (*InvalidationReport, error) {
	_, m := e.Scene.MediaByID(mediaID)
	if m == nil {
		return nil, &Fault{Code: EPara, Path: "media_id", MediaID: mediaID, Actual: mediaID, Expected: "existing medium", Message: "cannot invalidate unknown medium"}
	}
	g := e.DependencyGraph()
	if g == nil {
		return nil, &Fault{Code: ECache, Path: "dependency_graph", Message: "render the scene before invalidating"}
	}
	return g.Invalidate(DensitySource(mediaID, voxel)), nil
}

func (e *Engine) InvalidateCoefficient(mediaID string) (*InvalidationReport, error) {
	if _, m := e.Scene.MediaByID(mediaID); m == nil {
		return nil, &Fault{Code: EPara, Path: "media_id", MediaID: mediaID, Actual: mediaID, Expected: "existing medium", Message: "cannot invalidate unknown medium"}
	}
	g := e.DependencyGraph()
	if g == nil {
		return nil, &Fault{Code: ECache, Path: "dependency_graph", Message: "render the scene before invalidating"}
	}
	return g.Invalidate(CoefficientSource(mediaID)), nil
}

func (e *Engine) InvalidateCamera() (*InvalidationReport, error) {
	g := e.DependencyGraph()
	if g == nil {
		return nil, &Fault{Code: ECache, Path: "dependency_graph", Message: "render the scene before invalidating"}
	}
	return g.Invalidate(CameraSource(e.Scene.CameraHash())), nil
}

func (e *Engine) InvalidateRecipe() (*InvalidationReport, error) {
	g := e.DependencyGraph()
	if g == nil {
		return nil, &Fault{Code: ECache, Path: "dependency_graph", Message: "render the scene before invalidating"}
	}
	return g.Invalidate(RecipeSource(e.Scene.RecipeHash())), nil
}

// EditDensity applies a deterministic voxel edit after producing the
// invalidation report from the previous dependency graph.
func (e *Engine) EditDensity(mediaID string, voxel int, value int32) (*InvalidationReport, error) {
	_, m := e.Scene.MediaByID(mediaID)
	if m == nil || voxel < 0 || voxel >= len(m.Density) {
		return nil, &Fault{Code: EPara, Path: "density_voxel", MediaID: mediaID, Actual: voxel, Expected: fmt.Sprintf("0..%d", len(m.Density)-1), Message: "density voxel index out of range"}
	}
	if value < 0 || value > DensityMax {
		return nil, &Fault{Code: EDecode, Path: "density_voxel.value", MediaID: mediaID, Actual: value, Expected: fmt.Sprintf("0..%d", DensityMax), Message: "invalid density value"}
	}
	rep, err := e.InvalidateDensity(mediaID, voxel)
	if err != nil {
		return nil, err
	}
	m.Density[voxel] = value
	return rep, nil
}

func (e *Engine) EditCoefficient(mediaID string, absorptionFP, scatteringFP int64) (*InvalidationReport, error) {
	_, m := e.Scene.MediaByID(mediaID)
	if m == nil {
		return nil, &Fault{Code: EPara, Path: "media_id", MediaID: mediaID, Actual: mediaID, Expected: "existing medium", Message: "unknown medium"}
	}
	if absorptionFP < 0 || scatteringFP < 0 {
		return nil, &Fault{Code: EPara, Path: "coefficients", MediaID: mediaID, Actual: []int64{absorptionFP, scatteringFP}, Expected: ">=0", Message: "negative coefficient"}
	}
	rep, err := e.InvalidateCoefficient(mediaID)
	if err != nil {
		return nil, err
	}
	m.AbsorptionFP, m.ScatteringFP = absorptionFP, scatteringFP
	return rep, nil
}
