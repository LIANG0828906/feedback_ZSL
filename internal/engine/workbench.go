// Package engine orchestrates flattening, stroke-band generation,
// rasterization, segment caching, invalidation and parallel rendering.
package engine

import (
	"fmt"
	"sort"
	"sync"

	"strokeforge/internal/model"
	"strokeforge/internal/raster"
)

// Fault kinds understood by the injection pipeline.
const (
	FaultSelfIntersect   = "self-intersection"
	FaultZeroLength      = "zero-length"
	FaultDensityOverflow = "density-overflow"
	FaultStageFailure    = "stage-failure"
)

// InvalidationEntry explains why one stroke segment was marked dirty.
type InvalidationEntry struct {
	GlyphID    string `json:"glyph_id"`
	SegmentID  int    `json:"segment_id"`
	Reason     string `json:"reason"`
	Direct     bool   `json:"direct"`
	Downstream bool   `json:"downstream"`
	Parameter  string `json:"parameter"`
}

// DiffResult locates the first divergence between incremental and full paths.
type DiffResult struct {
	OK               bool   `json:"ok"`
	PixelDifference  int    `json:"pixel_difference"`
	DensityMismatch  int    `json:"density_mismatch"`
	SequenceMismatch bool   `json:"sequence_mismatch"`
	FirstGlyphID     string `json:"first_glyph_id"`
	FirstSegmentID   int    `json:"first_segment_id"`
	FirstX           int    `json:"first_x"`
	FirstY           int    `json:"first_y"`
	Message          string `json:"message"`
}

// PoolConfig bounds parallelism and per-worker buffering.
type PoolConfig struct {
	MaxWorkers     int
	MaxBufferBytes int
}

// DefaultPoolConfig matches the acceptance demonstration.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{MaxWorkers: 4, MaxBufferBytes: 1 << 20}
}

// GlyphRuntime holds the flattened geometry and per-segment records.
type GlyphRuntime struct {
	Glyph       *model.Glyph
	ContentHash string
	ContourHash map[string]string
	Frames      raster.Frame
	Flattened   map[string][]model.FPoint
	Records     map[int]*SegmentRecord
	Mask        *raster.Mask
	// SegOrder is the deterministic global commit order within the glyph.
	SegOrder []int
}

// Workbench is the stateful rendering engine.
type Workbench struct {
	mu           sync.Mutex
	doc          *model.Document
	paramVersion string
	runtimes     map[string]*GlyphRuntime
	pool         PoolConfig
	faults       map[string]map[int]string // glyph -> segment -> fault kind
	// peakBuffer is the largest per-worker allocation observed.
	peakBuffer int
}

// NewWorkbench validates pool limits and constructs an engine.
func NewWorkbench(doc *model.Document, pool PoolConfig) (*Workbench, error) {
	if pool.MaxWorkers <= 0 || pool.MaxWorkers > 16 {
		return nil, model.NewError(model.EPara,
			fmt.Sprintf("max workers %d out of range [1,16]", pool.MaxWorkers)).
			WithField("max_workers")
	}
	if pool.MaxBufferBytes < 4096 || pool.MaxBufferBytes > 64<<20 {
		return nil, model.NewError(model.EPara,
			fmt.Sprintf("worker buffer %d out of range", pool.MaxBufferBytes)).
			WithField("max_buffer_bytes")
	}
	wb := &Workbench{
		doc:          doc,
		paramVersion: ParamsHash(doc.StrokeParams),
		runtimes:     map[string]*GlyphRuntime{},
		pool:         pool,
		faults:       map[string]map[int]string{},
	}
	if err := wb.prepareRuntimes(); err != nil {
		return nil, err
	}
	return wb, nil
}

// Doc exposes the underlying document.
func (wb *Workbench) Doc() *model.Document { return wb.doc }

// ParamVersion exposes the current parameter hash.
func (wb *Workbench) ParamVersion() string { return wb.paramVersion }

func (wb *Workbench) prepareRuntimes() error {
	for gi := range wb.doc.Glyphs {
		g := &wb.doc.Glyphs[gi]
		flat := map[string][]model.FPoint{}
		var bbox model.BBox
		first := true
		for ci := range g.Contours {
			pts, err := FlattenContour(g.Contours[ci], wb.doc.StrokeParams.Tolerance)
			if err != nil {
				return err
			}
			flat[g.Contours[ci].ID] = pts
			b := BoundsOf(pts)
			if first {
				bbox, first = b, false
			} else {
				bbox = unionBBoxLocal(bbox, b)
			}
		}
		frame := raster.NewFrame(bbox, wb.doc.StrokeParams)
		order := make([]int, 0, len(g.StrokeSegments))
		for _, s := range g.StrokeSegments {
			order = append(order, s.ID)
		}
		sort.Ints(order)
		wb.runtimes[g.ID] = &GlyphRuntime{
			Glyph:       g,
			ContentHash: ContentHash(g),
			ContourHash: contourHashes(g),
			Frames:      frame,
			Flattened:   flat,
			Records:     map[int]*SegmentRecord{},
			Mask:        raster.NewMask(frame),
			SegOrder:    order,
		}
	}
	return nil
}

func contourHashes(g *model.Glyph) map[string]string {
	out := map[string]string{}
	for i := range g.Contours {
		out[g.Contours[i].ID] = ContourHash(&g.Contours[i])
	}
	return out
}

func unionBBoxLocal(a, b model.BBox) model.BBox {
	return model.BBox{
		MinX: minFloat(a.MinX, b.MinX),
		MinY: minFloat(a.MinY, b.MinY),
		MaxX: maxFloat(a.MaxX, b.MaxX),
		MaxY: maxFloat(a.MaxY, b.MaxY),
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// SegmentByID looks up the declared segment descriptor.
func (wb *Workbench) SegmentByID(glyphID string, segID int) (model.StrokeSegment, *GlyphRuntime, error) {
	rt, ok := wb.runtimes[glyphID]
	if !ok {
		return model.StrokeSegment{}, nil, model.NewError(model.ESchema, "unknown glyph: "+glyphID).
			WithField("glyph_id")
	}
	for _, s := range rt.Glyph.StrokeSegments {
		if s.ID == segID {
			return s, rt, nil
		}
	}
	return model.StrokeSegment{}, nil, model.NewError(model.ESchema, "unknown stroke segment").
		WithLocation(glyphID, segID).WithField("segment_id")
}

// validateBounds ensures the generated band stays inside the padded bbox
// envelope declared by the frame.
func (wb *Workbench) validateBounds(rt *GlyphRuntime, band StrokeBand) error {
	lim := wb.doc.StrokeParams.BBoxPadding
	b := rt.Frames.BBox
	for _, p := range band.Outline.Points {
		if p.X < b.MinX-lim || p.X > b.MaxX+lim || p.Y < b.MinY-lim || p.Y > b.MaxY+lim {
			return model.NewError(model.EBound,
				fmt.Sprintf("stroke band point (%.2f,%.2f) outside padded bbox", p.X, p.Y)).
				WithLocation(rt.Glyph.ID, band.SegmentID).WithField("bbox")
		}
	}
	return nil
}

// renderSegment executes the per-segment pipeline and returns a fresh record.
func (wb *Workbench) renderSegment(rt *GlyphRuntime, seg model.StrokeSegment) (*SegmentRecord, error) {
	glyphID := rt.Glyph.ID
	if fault, ok := wb.faults[glyphID][seg.ID]; ok {
		switch fault {
		case FaultSelfIntersect:
			return nil, model.NewError(model.ESelf, "injected skeleton self-intersection").
				WithLocation(glyphID, seg.ID)
		case FaultZeroLength:
			return nil, model.NewError(model.EZlen, "injected zero-length segment").
				WithLocation(glyphID, seg.ID)
		case FaultDensityOverflow:
			return nil, model.NewError(model.EDens, "injected density overflow").
				WithLocation(glyphID, seg.ID)
		case FaultStageFailure:
			return nil, model.NewError(model.EStage, "injected internal stage failure").
				WithLocation(glyphID, seg.ID)
		}
	}
	arc := BuildArcTable(rt.Flattened[seg.ContourID])
	poly := arc.Slice(seg.StartRatio, seg.EndRatio)
	if idx := zeroLengthOpenEdge(poly); idx >= 0 {
		return nil, model.NewError(model.EZlen,
			fmt.Sprintf("zero-length edge at index %d", idx)).
			WithLocation(glyphID, seg.ID)
	}
	if hit, i, j := selfIntersectOpen(poly); hit {
		return nil, model.NewError(model.ESelf,
			fmt.Sprintf("self-intersection between edges %d and %d", i, j)).
			WithLocation(glyphID, seg.ID)
	}
	band, err := BuildStrokeBand(seg.ID, poly, wb.doc.StrokeParams)
	if err != nil {
		return nil, err
	}
	if err := wb.validateBounds(rt, band); err != nil {
		return nil, err
	}
	mask := raster.NewMask(rt.Frames)
	bufSize := len(mask.Pix)
	if bufSize > wb.pool.MaxBufferBytes {
		return nil, model.NewError(model.EPara,
			fmt.Sprintf("segment mask buffer %d exceeds per-worker limit %d", bufSize, wb.pool.MaxBufferBytes)).
			WithLocation(glyphID, seg.ID).WithField("max_buffer_bytes")
	}
	mask.CompositeBand(band, rt.Frames)
	// Densities at five evenly spaced sample positions along the segment.
	densities := make([]int, 5)
	for k := 0; k < 5; k++ {
		r := seg.StartRatio + (float64(k)+0.5)/5*(seg.EndRatio-seg.StartRatio)
		p := arc.AtRatio(r)
		d, err := mask.DensityAt(p, rt.Frames)
		if err != nil {
			return nil, model.AttachLocation(err, glyphID, seg.ID)
		}
		if d < 0 || d > 255 {
			return nil, model.NewError(model.EDens, "density outside byte range").
				WithLocation(glyphID, seg.ID)
		}
		densities[k] = d
	}
	upstreamKeys := make([]string, 0, len(seg.Upstream))
	for _, up := range seg.Upstream {
		rec := rt.Records[up]
		if rec == nil {
			return nil, model.NewError(model.EStage, "upstream segment not committed").
				WithLocation(glyphID, seg.ID)
		}
		upstreamKeys = append(upstreamKeys, rec.CacheKey)
	}
	key := SegmentCacheKey(rt.ContourHash[seg.ContourID], wb.paramVersion, seg, upstreamKeys)
	return &SegmentRecord{
		GlyphID:      glyphID,
		SegmentID:    seg.ID,
		ContourID:    seg.ContourID,
		State:        StatePending,
		CacheKey:     key,
		Band:         band,
		Mask:         mask,
		Densities:    densities,
		Boundary:     band.Boundary,
		ParamVersion: wb.paramVersion,
		ContentHash:  rt.ContentHash,
		ContourHash:  rt.ContourHash[seg.ContourID],
		Algorithm:    model.AlgorithmVersion,
	}, nil
}

// InjectFault registers a fault for a segment.
func (wb *Workbench) InjectFault(glyphID string, segID int, kind string) error {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	if _, rt, err := wb.SegmentByID(glyphID, segID); err != nil {
		return err
	} else {
		_ = rt
	}
	switch kind {
	case FaultSelfIntersect, FaultZeroLength, FaultDensityOverflow, FaultStageFailure:
	default:
		return model.NewError(model.EPara, "unknown fault kind: "+kind).WithField("fault")
	}
	if wb.faults[glyphID] == nil {
		wb.faults[glyphID] = map[int]string{}
	}
	wb.faults[glyphID][segID] = kind
	return nil
}

// ClearFaults removes every injected fault.
func (wb *Workbench) ClearFaults() {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	wb.faults = map[string]map[int]string{}
}
