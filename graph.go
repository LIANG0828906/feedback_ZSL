package dialogue

import "fmt"

// GraphNode is a clip, parameter or frame-interval cache node.
type GraphNode struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	ClipID   string `json:"clip_id,omitempty"`
	RangeID  string `json:"range_id,omitempty"`
	CacheKey string `json:"cache_key,omitempty"`
}

// GraphEdge is a deterministic dependency edge.
type GraphEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// DependencyGraph is the clip -> feature -> calibration DAG plus parameters.
type DependencyGraph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// InvalidationPlan is the precise rebuild work order.
type InvalidationPlan struct {
	Reason                   string             `json:"reason"`
	AffectedClips            []string           `json:"affected_clips"`
	FrozenClips              []string           `json:"frozen_clips,omitempty"`
	FeatureRebuildRanges     map[string]bool    `json:"feature_rebuild_ranges"`
	InvalidNodeIDs           []string           `json:"invalid_node_ids"`
	InvalidCacheKeys         []string           `json:"invalid_cache_keys"`
	RebuiltFeatureFrames     []string           `json:"rebuilt_feature_frames,omitempty"`
	RebuiltCalibrationFrames []string           `json:"rebuilt_calibration_frames,omitempty"`
	RetainedFeatureFrames    []string           `json:"retained_feature_frames,omitempty"`
	Attribution              []FrameAttribution `json:"attribution,omitempty"`
	Steps                    []string           `json:"steps,omitempty"`
}

func newPlan(reason string) *InvalidationPlan {
	return &InvalidationPlan{
		Reason:               reason,
		AffectedClips:        make([]string, 0),
		FeatureRebuildRanges: make(map[string]bool),
		InvalidNodeIDs:       make([]string, 0),
		InvalidCacheKeys:     make([]string, 0),
		Steps:                make([]string, 0),
	}
}

func (p *InvalidationPlan) ShouldRebuildFeatureRange(rangeID string) bool {
	return p != nil && p.FeatureRebuildRanges[rangeID]
}

func (p *InvalidationPlan) addAffectedClip(id string) {
	for _, existing := range p.AffectedClips {
		if existing == id {
			return
		}
	}
	p.AffectedClips = append(p.AffectedClips, id)
}

// BuildDependencyGraph constructs the current DAG.
func BuildDependencyGraph(p *Project) *DependencyGraph {
	g := &DependencyGraph{Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	g.Nodes = append(g.Nodes,
		GraphNode{ID: "param:frame_rate", Kind: "parameter"},
		GraphNode{ID: "param:target_loudness", Kind: "parameter"},
	)
	for _, clip := range p.Clips {
		g.Nodes = append(g.Nodes,
			GraphNode{ID: "clip:" + clip.ID, Kind: "clip", ClipID: clip.ID},
			GraphNode{ID: "param:gain:" + clip.ID, Kind: "parameter", ClipID: clip.ID},
		)
		g.Edges = append(g.Edges,
			GraphEdge{From: "param:frame_rate", To: "clip:" + clip.ID, Reason: "quantization"},
		)
		for _, interval := range QuantizeClip(p, clip) {
			fkey := featureCacheKey(p, clip, interval).String()
			ckey := calibrationCacheKey(p, clip, interval, fkey).String()
			featureID := "feature:" + interval.RangeID
			calibID := "calibration:" + interval.RangeID
			g.Nodes = append(g.Nodes,
				GraphNode{ID: featureID, Kind: "feature", ClipID: clip.ID, RangeID: interval.RangeID, CacheKey: fkey},
				GraphNode{ID: calibID, Kind: "calibration", ClipID: clip.ID, RangeID: interval.RangeID, CacheKey: ckey},
			)
			g.Edges = append(g.Edges,
				GraphEdge{From: "clip:" + clip.ID, To: featureID, Reason: "pcm_content"},
				GraphEdge{From: "param:frame_rate", To: featureID, Reason: "frame_quantization"},
				GraphEdge{From: featureID, To: calibID, Reason: "upstream_feature_key"},
				GraphEdge{From: "param:gain:" + clip.ID, To: calibID, Reason: "clip_gain"},
				GraphEdge{From: "param:target_loudness", To: calibID, Reason: "target_loudness"},
			)
		}
	}
	return g
}

// ApplyClipGainEdit invalidates only this clip's calibration nodes.
func (e *Engine) ApplyClipGainEdit(clipID string, newGainBP int64) (*InvalidationPlan, *Error) {
	var clip *Clip
	for _, c := range e.Project.Clips {
		if c.ID == clipID {
			clip = c
		}
	}
	if clip == nil {
		return nil, &Error{Code: CodeState, Message: "unknown clip", ClipID: clipID}
	}
	if e.FrozenClips[clipID] {
		return nil, &Error{Code: CodeState, Message: "committed clip is frozen", ClipID: clipID}
	}
	if newGainBP < 0 || newGainBP > 100_000 {
		return nil, parameterError(clipID, "$.clips[].gain_bp", "gain_bp outside legal integer range [0,100000]")
	}
	plan := newPlan("clip_gain_changed")
	plan.addAffectedClip(clipID)
	intervals := QuantizeClip(e.Project, clip)
	for _, interval := range intervals {
		fkey := featureCacheKey(e.Project, clip, interval).String()
		ckey := calibrationCacheKey(e.Project, clip, interval, fkey).String()
		delete(e.CalibCache, ckey)
		plan.InvalidCacheKeys = append(plan.InvalidCacheKeys, ckey)
		plan.InvalidNodeIDs = append(plan.InvalidNodeIDs, "calibration:"+interval.RangeID)
	}
	plan.Steps = []string{
		fmt.Sprintf("hash audio for %s: unchanged (%s)", clipID, shortKey(clip.AudioHash)),
		"retain feature cache keys and quantized frame table",
		"invalidate clip calibration keys because gain parameter hash changed",
		"recompute clip gain solver and downstream frame loudness",
	}
	clip.GainBP = newGainBP
	e.LastPlan = plan
	e.Graph = BuildDependencyGraph(e.Project)
	return plan, nil
}

// ApplyTargetEdit invalidates calibration nodes for all uncommitted clips.
func (e *Engine) ApplyTargetEdit(newTargetAmpMicro int64) (*InvalidationPlan, *Error) {
	if newTargetAmpMicro <= 0 || newTargetAmpMicro > LinearScale {
		return nil, parameterError("", "$.calibration.target_amp_micro", "target amplitude must be in (0,1e6]")
	}
	plan := newPlan("target_loudness_changed")
	for _, clip := range e.Project.Clips {
		if e.FrozenClips[clip.ID] {
			plan.FrozenClips = append(plan.FrozenClips, clip.ID)
			continue
		}
		plan.addAffectedClip(clip.ID)
		intervals := QuantizeClip(e.Project, clip)
		for _, interval := range intervals {
			fkey := featureCacheKey(e.Project, clip, interval).String()
			ckey := calibrationCacheKey(e.Project, clip, interval, fkey).String()
			delete(e.CalibCache, ckey)
			plan.InvalidCacheKeys = append(plan.InvalidCacheKeys, ckey)
			plan.InvalidNodeIDs = append(plan.InvalidNodeIDs, "calibration:"+interval.RangeID)
		}
	}
	loud, _, err := LinearToDBMilli(newTargetAmpMicro, "$.calibration.target_amp_micro")
	if err != nil {
		return nil, asError(err, "", CodeOvf)
	}
	e.Project.TargetAmpMicro = newTargetAmpMicro
	e.Project.TargetLoudMDB = loud
	plan.InvalidNodeIDs = append([]string{"param:target_loudness"}, plan.InvalidNodeIDs...)
	plan.Steps = []string{
		"content hashes and feature keys unchanged",
		"invalidate target parameter and all uncommitted calibration keys",
		"re-solve clip adjustments and calibrated loudness",
	}
	if len(plan.FrozenClips) > 0 {
		plan.Steps = append(plan.Steps, "committed clips remain frozen and are not invalidated")
	}
	e.LastPlan = plan
	e.Graph = BuildDependencyGraph(e.Project)
	return plan, nil
}

// ApplyFrameRateEdit compares old/new frame intervals and invalidates only
// feature ranges whose concrete sample interval changed, plus calibration.
func (e *Engine) ApplyFrameRateEdit(newFPS int) (*InvalidationPlan, *Error) {
	if newFPS <= 0 {
		return nil, parameterError("", "$.frame_rate", "frame_rate must be positive")
	}
	oldFPS := e.Project.FrameRate
	plan := newPlan("frame_rate_changed")
	oldIntervalsByClip := make(map[string][]FrameInterval)
	for _, clip := range e.Project.Clips {
		oldIntervalsByClip[clip.ID] = QuantizeClip(e.Project, clip)
	}

	e.Project.FrameRate = newFPS
	for _, clip := range e.Project.Clips {
		if e.FrozenClips[clip.ID] {
			plan.FrozenClips = append(plan.FrozenClips, clip.ID)
			continue
		}
		oldIntervals := oldIntervalsByClip[clip.ID]
		oldByRange := make(map[string]FrameInterval, len(oldIntervals))
		for _, interval := range oldIntervals {
			oldByRange[interval.RangeID] = interval
		}
		newIntervals := QuantizeClip(e.Project, clip)
		newByRange := make(map[string]FrameInterval, len(newIntervals))
		clipChanged := false
		for _, interval := range newIntervals {
			newByRange[interval.RangeID] = interval
			if _, ok := oldByRange[interval.RangeID]; !ok {
				plan.FeatureRebuildRanges[interval.RangeID] = true
				plan.InvalidNodeIDs = append(plan.InvalidNodeIDs, "feature:"+interval.RangeID)
				clipChanged = true
			}
		}
		for _, oldInterval := range oldIntervals {
			if _, ok := newByRange[oldInterval.RangeID]; ok {
				continue
			}
			fkey := featureCacheKey(e.Project, clip, oldInterval).String()
			ckey := calibrationCacheKey(e.Project, clip, oldInterval, fkey).String()
			delete(e.FeatureCache, fkey)
			delete(e.CalibCache, ckey)
			plan.InvalidCacheKeys = append(plan.InvalidCacheKeys, fkey, ckey)
			plan.InvalidNodeIDs = append(plan.InvalidNodeIDs,
				"feature:"+oldInterval.RangeID, "calibration:"+oldInterval.RangeID)
			clipChanged = true
		}
		if clipChanged {
			plan.addAffectedClip(clip.ID)
		}
		if clip.Truth != nil {
			for _, check := range clip.Truth.SampleChecks {
				attr := AttributeSampleChange(check.SampleNo, oldFPS, newFPS, e.Project.SampleRate)
				if attr.Changed {
					plan.Attribution = append(plan.Attribution, attr)
				}
			}
		} else {
			attr := AttributeSampleChange(clip.StartSample, oldFPS, newFPS, e.Project.SampleRate)
			if attr.Changed {
				plan.Attribution = append(plan.Attribution, attr)
			}
		}
	}
	plan.Steps = []string{
		fmt.Sprintf("requantize boundaries at %d -> %d fps", oldFPS, newFPS),
		"compare concrete sample intervals rather than invalidating by clip alone",
		"rebuild missing feature cache keys",
		"invalidate downstream calibration for affected clips",
	}
	e.LastPlan = plan
	e.Graph = BuildDependencyGraph(e.Project)
	return plan, nil
}
