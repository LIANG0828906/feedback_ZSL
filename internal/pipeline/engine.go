// Package pipeline coordinates validated glyph skeletons, stroke-band
// generation, rasterization, dependency-aware invalidation, incremental redraw,
// fault isolation and checkpoint persistence.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"

	"glyphbrush/internal/checkpoint"
	"glyphbrush/internal/errs"
	"glyphbrush/internal/geom"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/model"
	"glyphbrush/internal/raster"
	"glyphbrush/internal/skeleton"
	"glyphbrush/internal/stroke"
)

const AlgorithmVersion = "glyphbrush.pipeline/v1"

type FaultKind string

const (
	FaultSelf    FaultKind = "self"
	FaultZero    FaultKind = "zlen"
	FaultDensity FaultKind = "dens"
	FaultStage   FaultKind = "stage"
)

type Fault struct {
	SegmentID int       `json:"segmentId"`
	Kind      FaultKind `json:"kind"`
}

type SegmentStatus string

const (
	StatusValid  SegmentStatus = "valid"
	StatusFailed SegmentStatus = "failed"
)

type Config struct {
	Params             stroke.Params
	Raster             raster.Config
	MaxParallel        int
	PolygonBufferLimit int
	Store              *checkpoint.Store
	Faults             []Fault
	SkeletonTolerance  float64
	DensityRange       model.DensityRange
	CoordinateRange    model.CoordinateRange
}

func DefaultConfig() Config {
	return Config{
		Params:             stroke.DefaultParams(),
		Raster:             raster.DefaultConfig(),
		MaxParallel:        4,
		PolygonBufferLimit: 200000,
		SkeletonTolerance:  skeleton.DefaultTolerance,
		DensityRange:       model.DensityRange{MinPPM: 0, MaxPPM: 1000},
		CoordinateRange:    model.CoordinateRange{MinX: -1 << 30, MinY: -1 << 30, MaxX: 1 << 30, MaxY: 1 << 30},
	}
}

type SegmentPlan struct {
	SegmentID    int              `json:"segmentId"`
	ContourIndex int              `json:"contourIndex"`
	CurveIndex   int              `json:"curveIndex"`
	ArcStart     float64          `json:"arcStart"`
	ArcEnd       float64          `json:"arcEnd"`
	Skeleton     []skeleton.Point `json:"skeleton"`
}

type SegmentResult struct {
	SegmentID    int                    `json:"segmentId"`
	ContourIndex int                    `json:"contourIndex"`
	CurveIndex   int                    `json:"curveIndex"`
	ArcStart     float64                `json:"arcStart"`
	ArcEnd       float64                `json:"arcEnd"`
	Status       SegmentStatus          `json:"status"`
	Error        *errs.Error            `json:"error,omitempty"`
	Band         *stroke.Band           `json:"band,omitempty"`
	Mask         raster.Mask            `json:"mask"`
	Densities    []raster.DensitySample `json:"densities,omitempty"`
}

type RunResult struct {
	GlyphID       string                 `json:"glyphId"`
	Params        stroke.Params          `json:"params"`
	ParamVersion  string                 `json:"paramVersion"`
	GlyphHash     string                 `json:"glyphHash"`
	Algorithm     string                 `json:"algorithmVersion"`
	Grid          *raster.Grid           `json:"grid"`
	Mask          raster.Mask            `json:"mask"`
	CompositeHash string                 `json:"compositeHash"`
	Densities     []raster.DensitySample `json:"densities"`
	Segments      []SegmentResult        `json:"segments"`
	Validation    *InvalidationReport    `json:"invalidation,omitempty"`
	Redrawn       []int                  `json:"redrawn"`
	Reused        []int                  `json:"reused"`
	Errors        []*errs.Error          `json:"errors,omitempty"`
}

type Engine struct {
	glyph        loader.Glyph
	params       stroke.Params
	rasterCfg    raster.Config
	grid         *raster.Grid
	plans        []SegmentPlan
	graph        *DependencyGraph
	cache        map[int]*SegmentResult
	faults       map[int]FaultKind
	stageUsed    map[int]bool
	maxPar       int
	polyLimit    int
	store        *checkpoint.Store
	tolerance    float64
	densityRange model.DensityRange
	coordRange   model.CoordinateRange
	opMu         sync.Mutex
	faultMu      sync.Mutex
}

func New(g loader.Glyph, cfg Config) (*Engine, error) {
	if err := cfg.Params.Validate(); err != nil {
		return nil, err.WithGlyph(g.Glyph.ID)
	}
	grid, e := raster.NewGrid(g.Glyph.Bounds.F(), cfg.Params.Width, cfg.Raster)
	if e != nil {
		return nil, e.WithGlyph(g.Glyph.ID)
	}
	if cfg.MaxParallel <= 0 {
		return nil, errs.New(errs.CodeParam, "maxParallel must be positive").WithParam("maxParallel").WithGlyph(g.Glyph.ID)
	}
	if cfg.PolygonBufferLimit <= 0 {
		return nil, errs.New(errs.CodeParam, "polygonBufferLimit must be positive").WithParam("polygonBufferLimit").WithGlyph(g.Glyph.ID)
	}
	tolerance := cfg.SkeletonTolerance
	if tolerance <= 0 {
		tolerance = skeleton.DefaultTolerance
	}
	skeletons, err := skeleton.Build(g.Glyph, tolerance)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSchema, err, "cannot build skeleton").WithGlyph(g.Glyph.ID)
	}
	g.Skeletons = skeletons
	plans := buildPlans(g)
	if len(plans) == 0 {
		return nil, errs.New(errs.CodeSchema, "glyph has no drawable stroke segments").WithGlyph(g.Glyph.ID)
	}
	engine := &Engine{
		glyph:        g,
		params:       cfg.Params,
		rasterCfg:    cfg.Raster,
		grid:         grid,
		plans:        plans,
		graph:        buildGraph(g, plans),
		cache:        make(map[int]*SegmentResult),
		faults:       make(map[int]FaultKind),
		stageUsed:    make(map[int]bool),
		maxPar:       cfg.MaxParallel,
		polyLimit:    cfg.PolygonBufferLimit,
		store:        cfg.Store,
		tolerance:    tolerance,
		densityRange: cfg.DensityRange,
		coordRange:   cfg.CoordinateRange,
	}
	if engine.densityRange.MaxPPM == 0 {
		engine.densityRange = model.DensityRange{MinPPM: 0, MaxPPM: 1000}
	}
	if engine.coordRange.MinX == 0 && engine.coordRange.MaxX == 0 {
		engine.coordRange = model.CoordinateRange{MinX: -1 << 30, MinY: -1 << 30, MaxX: 1 << 30, MaxY: 1 << 30}
	}
	for _, fault := range cfg.Faults {
		if _, ok := engine.plan(fault.SegmentID); !ok {
			return nil, errs.New(errs.CodeParam, "fault targets unknown segment %d", fault.SegmentID).
				WithParam("segmentId").WithGlyph(g.Glyph.ID)
		}
		if !validFaultKind(fault.Kind) {
			return nil, errs.New(errs.CodeParam, "unknown fault kind %q", fault.Kind).
				WithParam("fault").WithGlyph(g.Glyph.ID)
		}
		engine.faults[fault.SegmentID] = fault.Kind
	}
	return engine, nil
}

func validFaultKind(kind FaultKind) bool {
	switch kind {
	case FaultSelf, FaultZero, FaultDensity, FaultStage:
		return true
	default:
		return false
	}
}

func buildPlans(g loader.Glyph) []SegmentPlan {
	var plans []SegmentPlan
	id := 1
	for _, contour := range g.Skeletons {
		for _, run := range contour.Runs {
			plans = append(plans, SegmentPlan{
				SegmentID:    id,
				ContourIndex: contour.ContourIndex,
				CurveIndex:   run.CurveIndex,
				ArcStart:     run.StartArc,
				ArcEnd:       run.EndArc,
				Skeleton:     append([]skeleton.Point(nil), run.Points...),
			})
			id++
		}
	}
	sort.SliceStable(plans, func(i, j int) bool { return plans[i].SegmentID < plans[j].SegmentID })
	return plans
}

func (e *Engine) Params() stroke.Params   { return e.params }
func (e *Engine) Grid() *raster.Grid      { return e.grid }
func (e *Engine) Plans() []SegmentPlan    { return append([]SegmentPlan(nil), e.plans...) }
func (e *Engine) Graph() *DependencyGraph { return e.graph }
func (e *Engine) Glyph() loader.Glyph     { return e.glyph }

func (e *Engine) plan(id int) (SegmentPlan, bool) {
	for _, p := range e.plans {
		if p.SegmentID == id {
			return p, true
		}
	}
	return SegmentPlan{}, false
}

func (e *Engine) meta() checkpoint.Meta {
	return checkpoint.Meta{
		SchemaVersion:    checkpoint.SchemaVersion,
		GlyphID:          e.glyph.Glyph.ID,
		GlyphHash:        e.glyph.Glyph.Hash(),
		ParamVersion:     e.params.Version(),
		AlgorithmVersion: AlgorithmVersion,
		SegmentCount:     len(e.plans),
	}
}

// SetFaults replaces the deterministic fault map. Stage failures are one-shot
// per successful retry cycle; the other faults persist until cleared.
func (e *Engine) SetFaults(faults []Fault) *errs.Error {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	next := make(map[int]FaultKind, len(faults))
	for _, fault := range faults {
		if _, ok := e.plan(fault.SegmentID); !ok {
			return errs.New(errs.CodeParam, "fault targets unknown segment %d", fault.SegmentID).
				WithParam("segmentId").WithGlyph(e.glyph.Glyph.ID)
		}
		if !validFaultKind(fault.Kind) {
			return errs.New(errs.CodeParam, "unknown fault kind %q", fault.Kind).
				WithParam("fault").WithGlyph(e.glyph.Glyph.ID)
		}
		next[fault.SegmentID] = fault.Kind
	}
	e.faults = next
	e.stageUsed = make(map[int]bool)
	return nil
}

func (e *Engine) ClearFault(segmentID int) {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	delete(e.faults, segmentID)
	delete(e.stageUsed, segmentID)
}

func (e *Engine) faultFor(segmentID int) (FaultKind, bool) {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	kind, ok := e.faults[segmentID]
	if !ok {
		return "", false
	}
	if kind == FaultStage {
		if e.stageUsed[segmentID] {
			return "", false
		}
		e.stageUsed[segmentID] = true
	}
	return kind, true
}

func (e *Engine) renderBatch(ctx context.Context, plans []SegmentPlan, params stroke.Params) (map[int]*SegmentResult, []*errs.Error) {
	results := make([]*SegmentResult, len(plans))
	workers := e.maxPar
	if workers > len(plans) {
		workers = len(plans)
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				select {
				case <-ctx.Done():
					results[index] = e.failedResult(plans[index], errs.New(errs.CodeStage, "render canceled: %v", ctx.Err()).WithParam("stage"))
				default:
					results[index] = e.renderOne(plans[index], params)
				}
			}
		}()
	}
	for i := range plans {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	out := make(map[int]*SegmentResult, len(plans))
	var failures []*errs.Error
	for _, result := range results {
		out[result.SegmentID] = result
		if result.Error != nil {
			failures = append(failures, result.Error)
		}
	}
	sort.SliceStable(failures, func(i, j int) bool { return failures[i].SegmentID < failures[j].SegmentID })
	return out, failures
}

func (e *Engine) renderOne(plan SegmentPlan, params stroke.Params) *SegmentResult {
	result := &SegmentResult{
		SegmentID:    plan.SegmentID,
		ContourIndex: plan.ContourIndex,
		CurveIndex:   plan.CurveIndex,
		ArcStart:     plan.ArcStart,
		ArcEnd:       plan.ArcEnd,
		Status:       StatusValid,
	}
	fault, hasFault := e.faultFor(plan.SegmentID)
	if hasFault && fault == FaultZero {
		result.Status = StatusFailed
		result.Error = e.wrapSegmentError(errs.New(errs.CodeZero,
			"injected zero-length stroke segment").WithParam("arcLength"), plan)
		return result
	}
	points := make([]geom.Point, len(plan.Skeleton))
	for i, sp := range plan.Skeleton {
		points[i] = sp.Position
	}
	if len(points) < 2 || geom.PolylineLength(points) <= 1e-7 {
		result.Status = StatusFailed
		result.Error = e.wrapSegmentError(errs.New(errs.CodeZero,
			"stroke segment has zero arc length").WithParam("arcLength"), plan)
		return result
	}
	band, err := stroke.Build(points, params, e.glyph.Glyph.Bounds.F(), e.polyLimit)
	if err != nil {
		return e.failedResult(plan, err)
	}
	if hasFault && fault == FaultSelf {
		result.Status = StatusFailed
		result.Error = e.wrapSegmentError(errs.New(errs.CodeSelf,
			"injected self-intersection in skeleton-derived stroke band").WithParam("controlPoints"), plan)
		return result
	}
	mask, gridErr := raster.RasterizeBand(e.grid, band.Polygon)
	if gridErr != nil {
		return e.failedResult(plan, gridErr)
	}
	if hasFault && fault == FaultStage {
		result.Status = StatusFailed
		result.Error = e.wrapSegmentError(errs.New(errs.CodeStage,
			"injected transient failure while committing segment raster").WithParam("stage"), plan)
		return result
	}
	densities, densErr := raster.ComputeDensities(
		e.grid, mask, plan.Skeleton, band.BoundarySamples, params.Width, e.densityRange)
	if densErr != nil {
		return e.failedResult(plan, densErr)
	}
	for i := range densities {
		densities[i].SegmentID = plan.SegmentID
	}
	if hasFault && fault == FaultDensity {
		result.Status = StatusFailed
		result.Error = e.wrapSegmentError(errs.New(errs.CodeDens,
			"injected density overflow in segment samples").WithParam("density"), plan)
		return result
	}
	result.Band = band
	result.Mask = mask
	result.Densities = densities
	return result
}

func (e *Engine) failedResult(plan SegmentPlan, err error) *SegmentResult {
	return &SegmentResult{
		SegmentID:    plan.SegmentID,
		ContourIndex: plan.ContourIndex,
		CurveIndex:   plan.CurveIndex,
		ArcStart:     plan.ArcStart,
		ArcEnd:       plan.ArcEnd,
		Status:       StatusFailed,
		Error:        e.wrapSegmentError(err, plan),
	}
}

func (e *Engine) wrapSegmentError(err error, plan SegmentPlan) *errs.Error {
	e2, ok := errs.As(err)
	if !ok {
		e2 = errs.Wrap(errs.CodeStage, err, "segment rendering failed")
	}
	cp := *e2
	cp.GlyphID = e.glyph.Glyph.ID
	idx := plan.ContourIndex
	cp.ContourIndex = &idx
	cp.SegmentID = plan.SegmentID
	if cp.Param == "" {
		cp.Param = "strokeSegment"
	}
	return &cp
}

func (e *Engine) compose(result *RunResult, cache map[int]*SegmentResult) {
	ids := make([]int, 0, len(cache))
	for id := range cache {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var components []raster.Mask
	result.Segments = result.Segments[:0]
	result.Densities = nil
	for _, id := range ids {
		seg := cache[id]
		if seg == nil {
			continue
		}
		view := *seg
		result.Segments = append(result.Segments, view)
		if seg.Status != StatusValid {
			continue
		}
		components = append(components, seg.Mask)
		result.Densities = append(result.Densities, seg.Densities...)
	}
	result.Mask = raster.Composite(components)
	result.CompositeHash = hashMask(result.Mask)
	result.ParamVersion = e.params.Version()
	result.GlyphHash = e.glyph.Glyph.Hash()
	result.Algorithm = AlgorithmVersion
}

func hashMask(m raster.Mask) string {
	h := sha256.New()
	_, _ = h.Write([]byte(fmt.Sprintf("%dx%d:", m.Width, m.Height)))
	_, _ = h.Write(m.Data)
	return hex.EncodeToString(h.Sum(nil))
}

func batchError(failures []*errs.Error) error {
	if len(failures) == 0 {
		return nil
	}
	if len(failures) == 1 {
		return failures[0]
	}
	return &errs.BatchError{Errors: failures}
}

func cloneResult(src *SegmentResult) *SegmentResult {
	if src == nil {
		return nil
	}
	cp := *src
	if src.Band != nil {
		band := *src.Band
		band.Polygon = append([]geom.Point(nil), src.Band.Polygon...)
		band.Centerline = append([]geom.Point(nil), src.Band.Centerline...)
		band.BoundarySamples = append([]stroke.BoundarySample(nil), src.Band.BoundarySamples...)
		cp.Band = &band
	}
	cp.Mask.Data = append([]byte(nil), src.Mask.Data...)
	cp.Densities = append([]raster.DensitySample(nil), src.Densities...)
	if src.Error != nil {
		e := *src.Error
		cp.Error = &e
	}
	return &cp
}

func segmentIDs(plans []SegmentPlan) []int {
	ids := make([]int, len(plans))
	for i, p := range plans {
		ids[i] = p.SegmentID
	}
	return ids
}

func paramsJSON(p stroke.Params) string {
	raw, _ := json.Marshal(p)
	return string(raw)
}

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-7 }
