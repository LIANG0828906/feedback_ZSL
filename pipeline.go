package dialogue

import "sort"

// ClipOutput contains all derived data for one clip.
type ClipOutput struct {
	ClipID          string
	Intervals       []FrameInterval
	Features        []FrameFeature
	Calibration     *ClipCalibration
	Results         []FrameResult
	FeatureKeys     []string
	CalibrationKeys []string
}

// ProcessClip runs decode validation, frame extraction and calibration for one
// clip. A non-nil buffer is the worker's bounded sample buffer.
func ProcessClip(project *Project, clip *Clip, buffer []int16) (*ClipOutput, *Error) {
	if err := ValidatePCM(clip); err != nil {
		return nil, err.(*Error)
	}
	if clip.ForceStageFailure {
		return nil, &Error{
			Code:    CodeStage,
			Message: "injected single-clip stage failure",
			ClipID:  clip.ID,
			Field:   "force_stage_failure",
		}
	}
	intervals := QuantizeClip(project, clip)
	features := make([]FrameFeature, 0, len(intervals))
	featureKeys := make([]string, 0, len(intervals))
	for _, interval := range intervals {
		feature, err := ExtractFrame(project, clip, interval, buffer)
		if err != nil {
			if be, ok := err.(*Error); ok {
				return nil, be
			}
			return nil, &Error{Code: CodeStage, Message: err.Error(), ClipID: clip.ID}
		}
		features = append(features, feature)
		featureKeys = append(featureKeys, featureCacheKey(project, clip, interval).String())
	}
	calibration, gap, err := CalibrateClip(project, clip, features)
	if err != nil {
		if be, ok := err.(*Error); ok {
			return nil, be
		}
		return nil, &Error{Code: CodeStage, Message: err.Error(), ClipID: clip.ID}
	}
	if gap != nil {
		return nil, &Error{
			Code:    CodeLoud,
			Message: "target loudness is unreachable for clip",
			ClipID:  clip.ID,
			Details: LoudnessErrorDetails{Gaps: []LoudnessGap{*gap}, TotalBudgetBP: project.TotalBudgetBP},
		}
	}

	out := &ClipOutput{
		ClipID:          clip.ID,
		Intervals:       intervals,
		Features:        features,
		Calibration:     calibration,
		FeatureKeys:     featureKeys,
		CalibrationKeys: make([]string, 0, len(features)),
		Results:         make([]FrameResult, 0, len(features)),
	}
	calByFrame := make(map[int]CalibratedFrame, len(calibration.Frames))
	for _, cf := range calibration.Frames {
		calByFrame[cf.FrameNo] = cf
	}
	for i, feature := range features {
		cal := calByFrame[feature.FrameNo]
		ckey := calibrationCacheKey(project, clip, intervals[i], featureKeys[i]).String()
		out.CalibrationKeys = append(out.CalibrationKeys, ckey)
		out.Results = append(out.Results, FrameResult{
			ClipID:         clip.ID,
			FrameNo:        feature.FrameNo,
			RangeID:        feature.RangeID,
			Feature:        feature,
			Calibration:    cal,
			FeatureKey:     featureKeys[i],
			CalibrationKey: ckey,
		})
	}
	return out, nil
}

// Engine is the incremental cache and dependency-aware rebuild engine.
type Engine struct {
	Project       *Project
	FeatureCache  map[string]FrameFeature
	CalibCache    map[string]CalibratedFrame
	FrozenResults map[string][]FrameResult
	FrozenClips   map[string]bool
	Graph         *DependencyGraph
	LastPlan      *InvalidationPlan
}

func NewEngine(project *Project) *Engine {
	return &Engine{
		Project:       project,
		FeatureCache:  make(map[string]FrameFeature),
		CalibCache:    make(map[string]CalibratedFrame),
		FrozenResults: make(map[string][]FrameResult),
		FrozenClips:   make(map[string]bool),
		Graph:         BuildDependencyGraph(project),
	}
}

// BuildAll computes every frame and fills both cache levels.
func (e *Engine) BuildAll() ([]FrameResult, *Error) {
	buffer := make([]int16, FrameBufferCap)
	for _, clip := range e.Project.Clips {
		out, pErr := ProcessClip(e.Project, clip, buffer)
		if pErr != nil {
			return nil, pErr
		}
		for i := range out.Features {
			e.FeatureCache[out.FeatureKeys[i]] = out.Features[i]
			e.CalibCache[out.CalibrationKeys[i]] = out.Calibration.Frames[i]
		}
	}
	e.Graph = BuildDependencyGraph(e.Project)
	return e.Results(), nil
}

// FreezeClip pins the current results for a committed clip.
func (e *Engine) FreezeClip(clipID string) *Error {
	results, err := e.ClipResults(clipID)
	if err != nil {
		return err
	}
	e.FrozenClips[clipID] = true
	e.FrozenResults[clipID] = append([]FrameResult(nil), results...)
	return nil
}

func (e *Engine) IsFrozen(clipID string) bool { return e.FrozenClips[clipID] }

// Results materializes stable clip-ordered, frame-number-ordered results.
func (e *Engine) Results() []FrameResult {
	out := make([]FrameResult, 0)
	for _, clip := range e.Project.Clips {
		if e.FrozenClips[clip.ID] {
			out = append(out, e.FrozenResults[clip.ID]...)
			continue
		}
		if rows, err := e.ClipResults(clip.ID); err == nil {
			out = append(out, rows...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ClipID == out[j].ClipID {
			return out[i].FrameNo < out[j].FrameNo
		}
		return clipIndex(e.Project, out[i].ClipID) < clipIndex(e.Project, out[j].ClipID)
	})
	return out
}

func (e *Engine) ClipResults(clipID string) ([]FrameResult, *Error) {
	if e.FrozenClips[clipID] {
		return append([]FrameResult(nil), e.FrozenResults[clipID]...), nil
	}
	var clip *Clip
	for _, c := range e.Project.Clips {
		if c.ID == clipID {
			clip = c
			break
		}
	}
	if clip == nil {
		return nil, &Error{Code: CodeState, Message: "unknown clip", ClipID: clipID}
	}
	intervals := QuantizeClip(e.Project, clip)
	rows := make([]FrameResult, 0, len(intervals))
	for _, interval := range intervals {
		fkey := featureCacheKey(e.Project, clip, interval).String()
		feature, ok := e.FeatureCache[fkey]
		if !ok {
			return nil, &Error{Code: CodeStage, Message: "missing cached feature", ClipID: clipID, Field: fkey}
		}
		ckey := calibrationCacheKey(e.Project, clip, interval, fkey).String()
		cal, ok := e.CalibCache[ckey]
		if !ok {
			return nil, &Error{Code: CodeStage, Message: "missing cached calibration", ClipID: clipID, Field: ckey}
		}
		rows = append(rows, FrameResult{
			ClipID:         clip.ID,
			FrameNo:        interval.FrameNo,
			RangeID:        interval.RangeID,
			Feature:        feature,
			Calibration:    cal,
			FeatureKey:     fkey,
			CalibrationKey: ckey,
		})
	}
	return rows, nil
}

func clipIndex(p *Project, id string) int {
	for i, c := range p.Clips {
		if c.ID == id {
			return i
		}
	}
	return len(p.Clips)
}

// RebuildPlan recomputes only nodes named by an invalidation plan.
func (e *Engine) RebuildPlan(plan *InvalidationPlan) ([]FrameResult, *Error) {
	buffer := make([]int16, FrameBufferCap)
	for _, clipID := range plan.AffectedClips {
		if e.FrozenClips[clipID] {
			continue
		}
		var clip *Clip
		for _, c := range e.Project.Clips {
			if c.ID == clipID {
				clip = c
			}
		}
		if clip == nil {
			continue
		}
		intervals := QuantizeClip(e.Project, clip)
		features := make([]FrameFeature, 0, len(intervals))
		featureKeys := make([]string, 0, len(intervals))
		for _, interval := range intervals {
			fkey := featureCacheKey(e.Project, clip, interval).String()
			var feature FrameFeature
			if plan.ShouldRebuildFeatureRange(interval.RangeID) {
				f, pErr := ExtractFrame(e.Project, clip, interval, buffer)
				if pErr != nil {
					if be, ok := pErr.(*Error); ok {
						return nil, be
					}
					return nil, &Error{Code: CodeStage, Message: pErr.Error(), ClipID: clipID}
				}
				feature = f
				e.FeatureCache[fkey] = feature
				plan.RebuiltFeatureFrames = append(plan.RebuiltFeatureFrames, interval.RangeID)
			} else {
				cached, ok := e.FeatureCache[fkey]
				if !ok {
					f, pErr := ExtractFrame(e.Project, clip, interval, buffer)
					if pErr != nil {
						if be, ok := pErr.(*Error); ok {
							return nil, be
						}
						return nil, &Error{Code: CodeStage, Message: pErr.Error(), ClipID: clipID}
					}
					feature = f
					e.FeatureCache[fkey] = feature
					plan.RebuiltFeatureFrames = append(plan.RebuiltFeatureFrames, interval.RangeID+":cache-miss")
				} else {
					feature = cached
					plan.RetainedFeatureFrames = append(plan.RetainedFeatureFrames, interval.RangeID)
				}
			}
			features = append(features, feature)
			featureKeys = append(featureKeys, fkey)
		}
		cal, gap, err := CalibrateClip(e.Project, clip, features)
		if err != nil {
			if be, ok := err.(*Error); ok {
				return nil, be
			}
			return nil, &Error{Code: CodeStage, Message: err.Error(), ClipID: clipID}
		}
		if gap != nil {
			return nil, &Error{Code: CodeLoud, Message: "target unreachable after edit", ClipID: clipID, Details: *gap}
		}
		for i, frameCal := range cal.Frames {
			ckey := calibrationCacheKey(e.Project, clip, intervals[i], featureKeys[i]).String()
			e.CalibCache[ckey] = frameCal
			plan.RebuiltCalibrationFrames = append(plan.RebuiltCalibrationFrames, intervals[i].RangeID)
		}
	}
	e.LastPlan = plan
	e.Graph = BuildDependencyGraph(e.Project)
	return e.Results(), nil
}

// RunIncrementalEdit is a convenience wrapper used by tests and diagnostics.
func (e *Engine) RunIncrementalEdit(mutate func(p *Project) *InvalidationPlan) ([]FrameResult, *InvalidationPlan, *Error) {
	plan := mutate(e.Project)
	results, err := e.RebuildPlan(plan)
	if err != nil {
		return nil, plan, err
	}
	return results, plan, nil
}
