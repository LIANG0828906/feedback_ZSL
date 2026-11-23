package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"glyphbrush/internal/checkpoint"
	"glyphbrush/internal/errs"
	"glyphbrush/internal/geom"
	"glyphbrush/internal/raster"
	"glyphbrush/internal/stroke"
)

// FullRender renders every segment from an empty cache and does not read or
// modify the engine cache or the persistent checkpoint.
func (e *Engine) FullRender(ctx context.Context) (*RunResult, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.renderFresh(ctx, false)
}

// Warm renders every segment, atomically installs the cache and commits a
// complete checkpoint when every segment succeeds.
func (e *Engine) Warm(ctx context.Context) (*RunResult, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.renderFresh(ctx, true)
}

func (e *Engine) renderFresh(ctx context.Context, install bool) (*RunResult, error) {
	results, failures := e.renderBatch(ctx, e.plans, e.params)
	var persistErr error
	if install {
		e.cache = results
		if len(failures) == 0 {
			persistErr = e.persist()
		}
	}
	result := e.buildResult(results, nil, segmentIDs(e.plans), nil, failures)
	if persistErr != nil {
		pe := errs.Wrap(errs.CodeStage, persistErr, "checkpoint commit failed").WithParam("checkpoint")
		result.Errors = append(result.Errors, pe)
		failures = append(failures, pe)
	}
	return result, batchError(failures)
}

// Refresh recomposes the current cache without redrawing any segment.
func (e *Engine) Refresh() *RunResult {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.buildResult(e.cache, nil, nil, sortedKeys(e.cache), nil)
}

// UpdateParams applies a parameter change, invalidates only dependent
// segments, redraws them and atomically replaces cache entries.
func (e *Engine) UpdateParams(ctx context.Context, params stroke.Params) (*RunResult, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if err := params.Validate(); err != nil {
		return nil, err.WithGlyph(e.glyph.Glyph.ID)
	}
	changed := stroke.ChangedParams(e.params, params)
	if len(changed) == 0 && len(e.cache) == len(e.plans) {
		report := InvalidationReport{GlyphID: e.glyph.Glyph.ID, Empty: true}
		return e.buildResult(e.cache, &report, nil, sortedKeys(e.cache), nil), nil
	}
	e.params = params
	var report InvalidationReport
	var plans []SegmentPlan
	if len(e.cache) == 0 {
		report = e.allInvalidReport(changed, "cache is empty")
		plans = append([]SegmentPlan(nil), e.plans...)
	} else {
		report = e.graph.InvalidateParams(changed)
		plans = e.plansForReport(report)
	}
	nextCache := make(map[int]*SegmentResult, len(e.cache))
	for id, result := range e.cache {
		nextCache[id] = result
	}
	results, failures := e.renderBatch(ctx, plans, params)
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
	var reused []int
	redrawnSet := intSet(redrawn)
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

// Retry redraws exactly one segment. It is safe to call after a one-shot stage
// failure; deterministic faults must be cleared before retry can succeed.
func (e *Engine) Retry(ctx context.Context, segmentID int) (*RunResult, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	plan, ok := e.plan(segmentID)
	if !ok {
		return nil, errs.New(errs.CodeParam, "unknown stroke segment %d", segmentID).
			WithParam("segmentId").WithGlyph(e.glyph.Glyph.ID)
	}
	results, failures := e.renderBatch(ctx, []SegmentPlan{plan}, e.params)
	nextCache := make(map[int]*SegmentResult, len(e.cache)+1)
	for id, result := range e.cache {
		nextCache[id] = result
	}
	nextCache[segmentID] = results[segmentID]
	var persistErr error
	if len(failures) == 0 && len(nextCache) == len(e.plans) {
		persistErr = e.commitCache(nextCache)
	}
	e.cache = nextCache
	report := InvalidationReport{
		GlyphID:   e.glyph.Glyph.ID,
		Params:    []string{"retry"},
		Entries:   []InvalidationEntry{{SegmentID: segmentID, Param: "retry", Reason: "explicit segment retry"}},
		TopoOrder: []string{fmt.Sprintf("segment:%d", segmentID)},
	}
	result := e.buildResult(e.cache, &report, []int{segmentID}, nil, failures)
	if persistErr != nil {
		pe := errs.Wrap(errs.CodeStage, persistErr, "checkpoint commit failed").WithParam("checkpoint")
		result.Errors = append(result.Errors, pe)
		failures = append(failures, pe)
	}
	return result, batchError(failures)
}

// Restore loads a complete matching snapshot. Any version or geometry mismatch
// clears the cache and discards the whole snapshot.
func (e *Engine) Restore() (bool, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.cache = make(map[int]*SegmentResult)
	if e.store == nil {
		return false, checkpoint.ErrNoSnapshot
	}
	records, err := e.store.Load(e.meta())
	if err != nil {
		return false, err
	}
	if len(records) != len(e.plans) {
		e.store.Invalidate(e.glyph.Glyph.ID)
		return false, checkpoint.ErrVersionMismatch
	}
	next := make(map[int]*SegmentResult, len(records))
	for i, rec := range records {
		plan := e.plans[i]
		if rec.SegmentID != plan.SegmentID || rec.ContourIndex != plan.ContourIndex ||
			rec.CurveIndex != plan.CurveIndex || !almostEqual(rec.ArcStart, plan.ArcStart) ||
			!almostEqual(rec.ArcEnd, plan.ArcEnd) {
			e.store.Invalidate(e.glyph.Glyph.ID)
			return false, fmt.Errorf("%w: segment %d plan metadata differs", checkpoint.ErrVersionMismatch, rec.SegmentID)
		}
		if err := checkpoint.ValidateRecordGeometry(rec, e.grid.Width, e.grid.Height); err != nil {
			e.store.Invalidate(e.glyph.Glyph.ID)
			return false, err
		}
		if err := stroke.ValidateBoundary(rec.Band.Polygon, e.glyph.Glyph.Bounds.F(), e.params.Width/2); err != nil {
			e.store.Invalidate(e.glyph.Glyph.ID)
			return false, fmt.Errorf("%w: segment %d boundary: %v", checkpoint.ErrVersionMismatch, rec.SegmentID, err)
		}
		band := rec.Band
		band.Polygon = append([]geom.Point(nil), rec.Band.Polygon...)
		band.Centerline = append([]geom.Point(nil), rec.Band.Centerline...)
		band.BoundarySamples = append([]stroke.BoundarySample(nil), rec.Band.BoundarySamples...)
		mask := rec.Mask
		mask.Data = append([]byte(nil), rec.Mask.Data...)
		densities := append([]raster.DensitySample(nil), rec.Densities...)
		for _, sample := range densities {
			if sample.SegmentID != rec.SegmentID {
				e.store.Invalidate(e.glyph.Glyph.ID)
				return false, fmt.Errorf("%w: segment %d density ownership differs", checkpoint.ErrVersionMismatch, rec.SegmentID)
			}
		}
		next[rec.SegmentID] = &SegmentResult{
			SegmentID:    rec.SegmentID,
			ContourIndex: rec.ContourIndex,
			CurveIndex:   rec.CurveIndex,
			ArcStart:     rec.ArcStart,
			ArcEnd:       rec.ArcEnd,
			Status:       StatusValid,
			Band:         &band,
			Mask:         mask,
			Densities:    densities,
		}
	}
	e.cache = next
	return true, nil
}

// ClearCache drops in-memory segment results.
func (e *Engine) ClearCache() {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.cache = make(map[int]*SegmentResult)
}

func (e *Engine) buildResult(
	cache map[int]*SegmentResult,
	report *InvalidationReport,
	redrawn, reused []int,
	failures []*errs.Error,
) *RunResult {
	result := &RunResult{
		GlyphID:      e.glyph.Glyph.ID,
		Params:       e.params,
		ParamVersion: e.params.Version(),
		GlyphHash:    e.glyph.Glyph.Hash(),
		Algorithm:    AlgorithmVersion,
		Grid:         e.grid,
		Validation:   report,
		Redrawn:      copyIDs(redrawn),
		Reused:       copyIDs(reused),
		Errors:       append([]*errs.Error(nil), failures...),
	}
	e.compose(result, cache)
	return result
}

func copyIDs(ids []int) []int {
	out := make([]int, len(ids))
	copy(out, ids)
	return out
}

func (e *Engine) allInvalidReport(params []string, reason string) InvalidationReport {
	report := InvalidationReport{GlyphID: e.glyph.Glyph.ID, Params: append([]string(nil), params...)}
	for _, plan := range e.plans {
		report.Entries = append(report.Entries, InvalidationEntry{
			SegmentID:    plan.SegmentID,
			ContourIndex: plan.ContourIndex,
			CurveIndex:   plan.CurveIndex,
			ArcStart:     plan.ArcStart,
			ArcEnd:       plan.ArcEnd,
			Param:        firstOr(params, "cache"),
			Reason:       reason,
		})
		report.TopoOrder = append(report.TopoOrder, fmt.Sprintf("segment:%d", plan.SegmentID))
	}
	for _, layer := range e.graph.Layers {
		report.Layers = append(report.Layers, layer.ID)
		report.TopoOrder = append(report.TopoOrder, layer.ID)
	}
	report.Empty = len(report.Entries) == 0
	return report
}

func firstOr(values []string, fallback string) string {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}

func (e *Engine) plansForReport(report InvalidationReport) []SegmentPlan {
	ids := make(map[int]bool, len(report.Entries))
	for _, entry := range report.Entries {
		ids[entry.SegmentID] = true
	}
	var plans []SegmentPlan
	for _, plan := range e.plans {
		if ids[plan.SegmentID] {
			plans = append(plans, plan)
		}
	}
	return plans
}

func (e *Engine) commitCache(cache map[int]*SegmentResult) error {
	if e.store == nil {
		return nil
	}
	records := make([]checkpoint.Record, 0, len(e.plans))
	for _, plan := range e.plans {
		result, ok := cache[plan.SegmentID]
		if !ok || result.Status != StatusValid || result.Band == nil {
			return fmt.Errorf("checkpoint: segment %d is not valid", plan.SegmentID)
		}
		records = append(records, checkpoint.Record{
			SegmentID:    result.SegmentID,
			ContourIndex: result.ContourIndex,
			CurveIndex:   result.CurveIndex,
			ArcStart:     result.ArcStart,
			ArcEnd:       result.ArcEnd,
			Band:         *result.Band,
			Mask:         result.Mask,
			Densities:    result.Densities,
		})
	}
	return e.store.CommitAll(e.meta(), records)
}

func (e *Engine) persist() error {
	return e.commitCache(e.cache)
}

func (e *Engine) SnapshotMeta() checkpoint.Meta { return e.meta() }

func sortedKeys[V any](m map[int]V) []int {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func intSet(ids []int) map[int]bool {
	out := make(map[int]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// IsCheckpointMismatch reports whether err signals whole-snapshot invalidation.
func IsCheckpointMismatch(err error) bool {
	return errors.Is(err, checkpoint.ErrVersionMismatch) || errors.Is(err, checkpoint.ErrCorrupt)
}
