package vectorcore

import (
	"fmt"
	"sort"
)

func frameBoundsFor(scene *Scene, timeIndex int, cfg Config) (Bounds, error) {
	eval, err := EvaluateFrame(scene, timeIndex, cfg)
	if err != nil {
		return Bounds{}, err
	}
	bounds := EmptyBounds()
	for _, p := range eval.Paths {
		geo, err := buildPathGeometry(p, cfg)
		if err != nil {
			return Bounds{}, err
		}
		bounds.Union(geo.Bounds)
	}
	return bounds, nil
}

func containsInt(values []int, target int) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func dedupeInvalidations(items []Invalidation) []Invalidation {
	seen := make(map[string]bool, len(items))
	out := make([]Invalidation, 0, len(items))
	for _, item := range items {
		key := invalidationKey(item)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TimeIndex != out[j].TimeIndex {
			return out[i].TimeIndex < out[j].TimeIndex
		}
		if out[i].PathID != out[j].PathID {
			return out[i].PathID < out[j].PathID
		}
		return out[i].Stage < out[j].Stage
	})
	return out
}

func (e *Engine) invalidateTime(timeIndex, pathIndex int, geometryChanging, bboxChanged, compositeChanging bool, reason string) []Invalidation {
	targetID := e.scene.Paths[pathIndex].ID
	if geometryChanging || compositeChanging || bboxChanged {
		e.cache.removeMatching(func(entry *cacheEntry) bool {
			if entry.timeIndex != timeIndex {
				return false
			}
			switch entry.stage {
			case StageFlatten:
				return geometryChanging && entry.pathID == targetID
			case StageCoverage:
				return bboxChanged || geometryChanging && entry.pathID == targetID
			case StageComposite:
				return bboxChanged || (geometryChanging || compositeChanging) && entry.layer >= pathIndex
			default:
				return false
			}
		})
	}
	var out []Invalidation
	for pi, p := range e.scene.Paths {
		stages := map[Stage]bool{}
		if bboxChanged {
			stages[StageCoverage] = true
			stages[StageComposite] = true
		} else if geometryChanging && pi == pathIndex {
			stages[StageCoverage] = true
		}
		if geometryChanging && pi == pathIndex {
			stages[StageFlatten] = true
		}
		e.states.resetInvalidated(timeIndex, p.ID, stages)
	}
	// Reset composition for the edited layer and every downstream layer.
	if geometryChanging || compositeChanging || bboxChanged {
		for li := pathIndex; li < len(e.scene.Paths); li++ {
			e.states.resetInvalidated(timeIndex, e.scene.Paths[li].ID, map[Stage]bool{StageComposite: true})
		}
	}
	if geometryChanging {
		out = append(out, Invalidation{PathID: targetID, TimeIndex: timeIndex, Stage: StageFlatten, Reason: reason})
	}
	if bboxChanged {
		for _, p := range e.scene.Paths {
			out = append(out, Invalidation{PathID: p.ID, TimeIndex: timeIndex, Stage: StageCoverage, Reason: "bitmap bounds changed: " + reason})
			out = append(out, Invalidation{PathID: p.ID, TimeIndex: timeIndex, Stage: StageComposite, Reason: "bitmap bounds changed: " + reason})
		}
	} else {
		if geometryChanging {
			out = append(out, Invalidation{PathID: targetID, TimeIndex: timeIndex, Stage: StageCoverage, Reason: reason})
		}
		if geometryChanging || compositeChanging {
			for li := pathIndex; li < len(e.scene.Paths); li++ {
				out = append(out, Invalidation{PathID: e.scene.Paths[li].ID, TimeIndex: timeIndex, Stage: StageComposite, Reason: reason})
			}
		}
	}
	return out
}

// UpdateKeyframe edits one contour and invalidates only uncommitted sample
// times whose interpolation uses that keyframe, plus their downstream stages.
func (e *Engine) UpdateKeyframe(pathID string, keyframeIndex int, contour Contour) ([]Invalidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.rendering {
		return nil, &Error{Code: CodeState, Op: "updateKeyframe", PathID: pathID, Keyframe: keyframeIndex, TimeIndex: -1,
			Err: fmt.Errorf("scene edit during active render")}
	}
	pathIndex := -1
	for i := range e.scene.Paths {
		if e.scene.Paths[i].ID == pathID {
			pathIndex = i
			break
		}
	}
	if pathIndex < 0 {
		return nil, &Error{Code: CodeSchema, Op: "updateKeyframe", PathID: pathID, Keyframe: keyframeIndex, TimeIndex: -1,
			Err: fmt.Errorf("unknown path")}
	}
	if keyframeIndex < 0 || keyframeIndex >= len(e.scene.Paths[pathIndex].Keyframes) {
		return nil, &Error{Code: CodeParam, Op: "updateKeyframe", PathID: pathID, Field: "keyframeIndex", Keyframe: keyframeIndex, TimeIndex: -1,
			Err: fmt.Errorf("outside path keyframe range")}
	}
	if HashValue(e.scene.Paths[pathIndex].Keyframes[keyframeIndex].Contour) == HashValue(contour) {
		return nil, nil
	}
	next := e.scene.Clone()
	next.Paths[pathIndex].Keyframes[keyframeIndex].Contour = contour.Clone()
	if err := next.Validate(); err != nil {
		return nil, err
	}
	reason := fmt.Sprintf("keyframe %d changed", keyframeIndex)
	var invalidations []Invalidation
	for ti := range e.scene.SampleTimes {
		if e.resultFor(ti) != nil {
			continue
		}
		if !containsInt(KeyframeIndicesForTime(e.scene.Paths[pathIndex], e.scene.SampleTimes[ti]), keyframeIndex) {
			continue
		}
		oldBounds, err := frameBoundsFor(e.scene, ti, e.cfg)
		if err != nil {
			return nil, err
		}
		newBounds, err := frameBoundsFor(next, ti, e.cfg)
		if err != nil {
			return nil, err
		}
		invalidations = append(invalidations, e.invalidateTime(ti, pathIndex, true, oldBounds != newBounds, false, reason)...)
	}
	e.scene = next
	return dedupeInvalidations(invalidations), nil
}

// UpdatePaint edits stroke, color, opacity or blend parameters. Stroke width
// and join invalidate geometry; opacity and blend start at composition.
func (e *Engine) UpdatePaint(pathID string, paint Paint) ([]Invalidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.updatePaintLocked(pathID, paint)
}

func (e *Engine) updatePaintLocked(pathID string, paint Paint) ([]Invalidation, error) {
	if e.rendering {
		return nil, &Error{Code: CodeState, Op: "updatePaint", PathID: pathID, Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("scene edit during active render")}
	}
	pathIndex := -1
	for i := range e.scene.Paths {
		if e.scene.Paths[i].ID == pathID {
			pathIndex = i
			break
		}
	}
	if pathIndex < 0 {
		return nil, &Error{Code: CodeSchema, Op: "updatePaint", PathID: pathID, Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("unknown path")}
	}
	if err := paint.Validate(pathID); err != nil {
		return nil, err
	}
	oldPaint := e.scene.Paths[pathIndex].Paint
	if oldPaint == paint {
		return nil, nil
	}
	geometryChanging := oldPaint.Fill != paint.Fill || oldPaint.StrokeWidth != paint.StrokeWidth || oldPaint.Join != paint.Join
	compositeChanging := oldPaint.Color != paint.Color || oldPaint.Opacity != paint.Opacity || oldPaint.Blend != paint.Blend
	next := e.scene.Clone()
	next.Paths[pathIndex].Paint = paint
	if err := next.Validate(); err != nil {
		return nil, err
	}
	reason := "paint changed"
	if oldPaint.Fill != paint.Fill {
		reason = "fill flag changed"
	} else if oldPaint.StrokeWidth != paint.StrokeWidth {
		reason = "stroke width changed"
	} else if oldPaint.Join != paint.Join {
		reason = "stroke join changed"
	} else if oldPaint.Opacity != paint.Opacity {
		reason = "opacity changed"
	} else if oldPaint.Blend != paint.Blend {
		reason = "blend mode changed"
	} else if oldPaint.Color != paint.Color {
		reason = "color changed"
	}
	var invalidations []Invalidation
	for ti := range e.scene.SampleTimes {
		if e.resultFor(ti) != nil {
			continue
		}
		bboxChanged := false
		if geometryChanging {
			oldBounds, err := frameBoundsFor(e.scene, ti, e.cfg)
			if err != nil {
				return nil, err
			}
			newBounds, err := frameBoundsFor(next, ti, e.cfg)
			if err != nil {
				return nil, err
			}
			bboxChanged = oldBounds != newBounds
		}
		invalidations = append(invalidations, e.invalidateTime(ti, pathIndex, geometryChanging, bboxChanged, compositeChanging, reason)...)
	}
	e.scene = next
	return dedupeInvalidations(invalidations), nil
}

func (e *Engine) UpdateStroke(pathID string, width int64, join JoinMode) ([]Invalidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.scene.Paths {
		if e.scene.Paths[i].ID == pathID {
			paint := e.scene.Paths[i].Paint
			paint.StrokeWidth = width
			paint.Join = join
			return e.updatePaintLocked(pathID, paint)
		}
	}
	return nil, &Error{Code: CodeSchema, Op: "updateStroke", PathID: pathID, Keyframe: -1, TimeIndex: -1,
		Err: fmt.Errorf("unknown path")}
}
