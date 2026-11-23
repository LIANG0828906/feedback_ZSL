package pipeline

import (
	"context"
	"fmt"
	"sort"

	"glyphbrush/internal/errs"
	"glyphbrush/internal/geom"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/model"
	"glyphbrush/internal/skeleton"
)

type ControlEdit struct {
	ContourIndex int   `json:"contourIndex"`
	SegmentIndex int   `json:"segmentIndex"`
	ControlIndex int   `json:"controlIndex"`
	DX           int64 `json:"dx"`
	DY           int64 `json:"dy"`
}

// EditControl mutates one control point, rebuilds that glyph's skeletons and
// invalidates only segments whose arc interval changes: the edited curve and
// all later curves in the same contour. Other contours reuse cache entries.
func (e *Engine) EditControl(ctx context.Context, edit ControlEdit) (*RunResult, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if edit.ContourIndex < 0 || edit.ContourIndex >= len(e.glyph.Glyph.Contours) {
		return nil, errs.New(errs.CodeParam, "contourIndex is out of range").
			WithParam("contourIndex").WithGlyph(e.glyph.Glyph.ID)
	}
	contour := &e.glyph.Glyph.Contours[edit.ContourIndex]
	if edit.SegmentIndex < 0 || edit.SegmentIndex >= len(contour.Segments) {
		return nil, errs.New(errs.CodeParam, "segmentIndex is out of range").
			WithParam("segmentIndex").WithGlyph(e.glyph.Glyph.ID).WithContour(edit.ContourIndex)
	}
	segment := &contour.Segments[edit.SegmentIndex]
	if edit.ControlIndex < 0 || edit.ControlIndex >= len(segment.Control) {
		return nil, errs.New(errs.CodeParam, "controlIndex is out of range").
			WithParam("controlIndex").WithGlyph(e.glyph.Glyph.ID).WithContour(edit.ContourIndex)
	}
	if edit.DX == 0 && edit.DY == 0 {
		report := InvalidationReport{GlyphID: e.glyph.Glyph.ID, Empty: true}
		return e.buildResult(e.cache, &report, nil, sortedKeys(e.cache), nil), nil
	}
	oldPoint := segment.Control[edit.ControlIndex]
	newPoint := model.Point{X: oldPoint.X + edit.DX, Y: oldPoint.Y + edit.DY}
	if !e.coordRange.Contains(newPoint) {
		return nil, errs.New(errs.CodeParam,
			"edited control point (%d,%d) is outside declared coordinate range",
			newPoint.X, newPoint.Y).WithParam("controlPoint").WithGlyph(e.glyph.Glyph.ID).WithContour(edit.ContourIndex)
	}
	report := e.graph.InvalidateControl(edit.ContourIndex, edit.SegmentIndex)
	nextGlyph := cloneLoaderGlyph(e.glyph)
	nextGlyph.Glyph.Contours[edit.ContourIndex].Segments[edit.SegmentIndex].Control[edit.ControlIndex] = newPoint
	skeletons, err := skeleton.Build(nextGlyph.Glyph, e.tolerance)
	if err != nil {
		return nil, errs.Wrap(errs.CodeSchema, err, "cannot rebuild skeleton after control edit").
			WithGlyph(e.glyph.Glyph.ID).WithContour(edit.ContourIndex)
	}
	nextGlyph.Skeletons = skeletons
	nextPlans := buildPlans(nextGlyph)
	if len(nextPlans) != len(e.plans) {
		return nil, errs.New(errs.CodeSchema, "control edit changed stroke segment count").
			WithGlyph(e.glyph.Glyph.ID)
	}
	// Refresh arc ranges in the report after rebuilding cumulative arc lengths.
	newByID := make(map[int]SegmentPlan, len(nextPlans))
	for _, plan := range nextPlans {
		newByID[plan.SegmentID] = plan
	}
	for i := range report.Entries {
		plan := newByID[report.Entries[i].SegmentID]
		report.Entries[i].ArcStart = plan.ArcStart
		report.Entries[i].ArcEnd = plan.ArcEnd
	}
	e.glyph = nextGlyph
	e.plans = nextPlans
	e.graph = buildGraph(nextGlyph, nextPlans)

	plans := e.plansForReport(report)
	nextCache := make(map[int]*SegmentResult, len(e.cache))
	for id, result := range e.cache {
		nextCache[id] = result
	}
	results, failures := e.renderBatch(ctx, plans, e.params)
	redrawn := make([]int, 0, len(plans))
	for _, plan := range plans {
		redrawn = append(redrawn, plan.SegmentID)
		if result, ok := results[plan.SegmentID]; ok {
			nextCache[plan.SegmentID] = result
		}
	}
	var persistErr error
	if len(failures) == 0 && len(nextCache) == len(e.plans) {
		persistErr = e.commitCache(nextCache)
	}
	e.cache = nextCache
	redrawnSet := intSet(redrawn)
	var reused []int
	for id := range e.cache {
		if !redrawnSet[id] {
			reused = append(reused, id)
		}
	}
	sort.Ints(reused)
	result := e.buildResult(e.cache, &report, redrawn, reused, failures)
	if persistErr != nil {
		pe := errs.Wrap(errs.CodeStage, persistErr, "checkpoint commit failed").WithParam("checkpoint")
		result.Errors = append(result.Errors, pe)
		failures = append(failures, pe)
	}
	return result, batchError(failures)
}

func cloneLoaderGlyph(src loader.Glyph) loader.Glyph {
	var out loader.Glyph
	out.Truth = src.Truth
	out.Glyph.ID = src.Glyph.ID
	out.Glyph.Baseline = src.Glyph.Baseline
	out.Glyph.AdvanceWidth = src.Glyph.AdvanceWidth
	out.Glyph.Bounds = src.Glyph.Bounds
	for _, contour := range src.Glyph.Contours {
		c := model.Contour{Index: contour.Index, Closed: contour.Closed}
		for _, segment := range contour.Segments {
			s := model.Segment{Type: segment.Type, Start: segment.Start, End: segment.End}
			s.Control = append([]model.Point(nil), segment.Control...)
			c.Segments = append(c.Segments, s)
		}
		out.Glyph.Contours = append(out.Glyph.Contours, c)
	}
	out.Skeletons = append([]skeleton.Contour(nil), src.Skeletons...)
	return out
}

// SegmentGeometry exposes the current flattened skeleton of one segment,
// primarily for visualization and tests.
func (e *Engine) SegmentGeometry(segmentID int) ([]geom.Point, bool) {
	plan, ok := e.plan(segmentID)
	if !ok {
		return nil, false
	}
	points := make([]geom.Point, len(plan.Skeleton))
	for i, p := range plan.Skeleton {
		points[i] = p.Position
	}
	return points, true
}

func (e *Engine) String() string {
	return fmt.Sprintf("engine(%s, segments=%d, params=%s)", e.glyph.Glyph.ID, len(e.plans), paramsJSON(e.params))
}
