package loudness

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
)

type NodeKind string

const (
	NodeSource        NodeKind = "clip-source"
	NodeFrameEnvelope NodeKind = "frame-envelope"
	NodeCalibration   NodeKind = "gain-calibration"
)

// DependencyNode is one cache-keyed node in the clip/frame dependency graph.
type DependencyNode struct {
	Key                string   `json:"key"`
	Kind               NodeKind `json:"kind"`
	ClipID             int      `json:"clipId"`
	FrameIndex         int      `json:"frameIndex,omitempty"`
	RangeID            string   `json:"rangeId,omitempty"`
	StartSample        int64    `json:"startSample"`
	EndSample          int64    `json:"endSample"`
	ContentHash        string   `json:"contentHash"`
	QuantVersion       string   `json:"quantVersion"`
	LoudnessWeightQ8   int64    `json:"loudnessWeightQ8"`
	GainQ12            int64    `json:"gainQ12"`
	GainScale          int64    `json:"gainScale"`
	LoudnessScale      int64    `json:"loudnessScale"`
	TargetLoudnessDBQ8 int64    `json:"targetLoudnessDbQ8,omitempty"`
	Upstream           []string `json:"upstream"`
	CacheKey           string   `json:"cacheKey"`
	Valid              bool     `json:"valid"`
}

// DependencySnapshot is a diagnostic listing of the graph.
type DependencySnapshot struct {
	Nodes []DependencyNode `json:"nodes"`
}

// DependencyGraph records source->frame envelope->calibration dependencies.
type DependencyGraph struct {
	nodes        map[string]*DependencyNode
	clipFrames   map[int][]string
	clipCal      map[int]string
	quantVersion string
}

// BuildDependencyGraph derives all cache keys from content hashes, frame
// interval IDs, quantization version, and upstream keys.
func BuildDependencyGraph(project *Project) (*DependencyGraph, error) {
	if err := validateProjectBasics(project); err != nil {
		return nil, err
	}
	graph := &DependencyGraph{
		nodes:        make(map[string]*DependencyNode),
		clipFrames:   make(map[int][]string),
		clipCal:      make(map[int]string),
		quantVersion: project.Quantization.Version,
	}
	for _, clip := range project.Clips {
		pcm, err := GeneratePCM(project, clip)
		if err != nil {
			return nil, err
		}
		pcmHash := hashPCM(pcm)
		contentHash := hashClip(project, clip, pcmHash)
		sourceKey := "source:clip:" + strconv.Itoa(clip.ID) + ":" + contentHash
		sourceCache := MakeCacheKey(contentHash, sourceKey, graph.quantVersion, nil)
		sourceNode := &DependencyNode{
			Key: sourceKey, Kind: NodeSource, ClipID: clip.ID,
			StartSample: clip.StartSample, EndSample: clip.EndSample,
			ContentHash: contentHash, QuantVersion: graph.quantVersion,
			LoudnessWeightQ8: clip.LoudnessWeightQ8, GainQ12: clip.GainQ12,
			GainScale: project.Quantization.GainScale, LoudnessScale: project.Quantization.LoudnessScale,
			Upstream: nil, CacheKey: sourceCache, Valid: true,
		}
		graph.nodes[sourceKey] = sourceNode
		intervals, err := frameIntervals(project, clip)
		if err != nil {
			return nil, err
		}
		frameKeys := make([]string, 0, len(intervals))
		for index, interval := range intervals {
			rangeID := fmt.Sprintf("clip:%d/frame:%d/%d-%d", clip.ID, index, interval.start, interval.end)
			frameKey := "frame:" + strconv.Itoa(clip.ID) + ":" + strconv.Itoa(index)
			cacheKey := MakeCacheKey(contentHash, rangeID, graph.quantVersion, []string{sourceCache})
			node := &DependencyNode{
				Key: frameKey, Kind: NodeFrameEnvelope, ClipID: clip.ID, FrameIndex: index,
				RangeID: rangeID, StartSample: interval.start, EndSample: interval.end,
				ContentHash: contentHash, QuantVersion: graph.quantVersion,
				LoudnessWeightQ8: clip.LoudnessWeightQ8, GainQ12: clip.GainQ12,
				GainScale: project.Quantization.GainScale, LoudnessScale: project.Quantization.LoudnessScale,
				Upstream: []string{sourceCache}, CacheKey: cacheKey, Valid: true,
			}
			graph.nodes[frameKey] = node
			frameKeys = append(frameKeys, cacheKey)
		}
		graph.clipFrames[clip.ID] = append([]string(nil), frameKeys...)
		calParams := fmt.Sprintf("target=%d;session=%d;max=%d;tol=%d", project.TargetLoudnessDBQ8, project.SessionGainQ12, project.MaxGainQ12, project.LoudnessToleranceQ8)
		calKey := "cal:" + strconv.Itoa(clip.ID)
		calCache := MakeCacheKey(contentHash, calKey, graph.quantVersion, append(append([]string(nil), frameKeys...), "params:"+calParams))
		calNode := &DependencyNode{
			Key: calKey, Kind: NodeCalibration, ClipID: clip.ID,
			StartSample: clip.StartSample, EndSample: clip.EndSample,
			ContentHash: contentHash, QuantVersion: graph.quantVersion,
			LoudnessWeightQ8: clip.LoudnessWeightQ8, GainQ12: clip.GainQ12,
			GainScale: project.Quantization.GainScale, LoudnessScale: project.Quantization.LoudnessScale,
			TargetLoudnessDBQ8: project.TargetLoudnessDBQ8,
			Upstream:           append([]string(nil), frameKeys...), CacheKey: calCache, Valid: true,
		}
		graph.nodes[calKey] = calNode
		graph.clipCal[clip.ID] = calCache
	}
	return graph, nil
}

type frameInterval struct {
	start int64
	end   int64
}

func frameIntervals(project *Project, clip Clip) ([]frameInterval, error) {
	if err := ValidateFrameParameters(project.FrameLength, project.FrameShift); err != nil {
		return nil, err
	}
	duration := clip.DurationSamples()
	if duration <= 0 {
		return nil, &DomainError{Code: ESchema, Path: fmt.Sprintf("$.clips[%d]", indexOfClip(project, clip.ID)), Detail: "clip has non-positive duration"}
	}
	intervals := make([]frameInterval, 0, int((duration+project.FrameShift-1)/project.FrameShift))
	for localStart := int64(0); localStart < duration; localStart += project.FrameShift {
		localEnd := localStart + project.FrameLength
		if localEnd > duration {
			localEnd = duration
		}
		intervals = append(intervals, frameInterval{
			start: clip.StartSample + localStart,
			end:   clip.StartSample + localEnd,
		})
	}
	return intervals, nil
}

// MakeCacheKey is the single cache-key formula used by graph, checkpoints, and
// comparison diagnostics.
func MakeCacheKey(contentHash, intervalID, quantVersion string, upstream []string) string {
	hasher := sha256.New()
	writeField(hasher, "content", contentHash)
	writeField(hasher, "interval", intervalID)
	writeField(hasher, "quant", quantVersion)
	sorted := append([]string(nil), upstream...)
	sort.Strings(sorted)
	for _, key := range sorted {
		writeField(hasher, "upstream", key)
	}
	return fmt.Sprintf("%x", hasher.Sum(nil))
}

// ChangeKind identifies designer edits that can invalidate cached results.
type ChangeKind string

const (
	ChangeGain      ChangeKind = "clip-gain"
	ChangeTarget    ChangeKind = "target-loudness"
	ChangeFrameRate ChangeKind = "frame-rate"
)

// Change is one explicit designer edit.
type Change struct {
	Kind               ChangeKind `json:"kind"`
	ClipID             int        `json:"clipId,omitempty"`
	GainQ12            int64      `json:"gainQ12,omitempty"`
	TargetLoudnessDBQ8 int64      `json:"targetLoudnessDbQ8,omitempty"`
	FrameLength        int64      `json:"frameLength,omitempty"`
	FrameShift         int64      `json:"frameShift,omitempty"`
}

// InvalidationPhase says which cached node kind must be recomputed.
type InvalidationPhase string

const (
	PhaseEnvelope    InvalidationPhase = "envelope+calibration"
	PhaseCalibration InvalidationPhase = "calibration"
)

// Invalidation is one precise clip/frame interval report.
type Invalidation struct {
	ClipID      int               `json:"clipId"`
	StartSample int64             `json:"startSample"`
	EndSample   int64             `json:"endSample"`
	Phase       InvalidationPhase `json:"phase"`
	Reason      string            `json:"reason"`
}

// ApplyChange mutates an internal project without rebuilding its graph.
func ApplyChange(project *Project, change Change) error {
	if project == nil {
		return &DomainError{Code: ESchema, Path: "$", Detail: "project is nil"}
	}
	switch change.Kind {
	case ChangeGain:
		clip, ok := project.ClipByID(change.ClipID)
		if !ok {
			return &DomainError{Code: ESchema, Path: "$.clips", Field: "clipId", Detail: fmt.Sprintf("unknown clip %d", change.ClipID)}
		}
		if change.GainQ12 <= 0 {
			return &DomainError{Code: EPara, Path: fmt.Sprintf("$.clips[%d].gainQ12", indexOfClip(project, clip.ID)), Field: "gainQ12", LegalMin: 1, LegalMax: project.MaxGainQ12}
		}
		old := clip.GainQ12
		clip.GainQ12 = change.GainQ12
		project.Provenance.Traces = append(project.Provenance.Traces, FieldTrace{
			SourceVersion: project.FormatVersion,
			SourcePath:    fmt.Sprintf("$.clips[%d].gainQ12", indexOfClip(project, clip.ID)),
			InternalPath:  fmt.Sprintf("$.clips[%d].gainQ12", indexOfClip(project, clip.ID)),
			Kind:          TraceConvert, OldValue: strconv.FormatInt(old, 10), NewValue: strconv.FormatInt(change.GainQ12, 10),
		})
	case ChangeTarget:
		old := project.TargetLoudnessDBQ8
		project.TargetLoudnessDBQ8 = change.TargetLoudnessDBQ8
		project.Provenance.Traces = append(project.Provenance.Traces, FieldTrace{
			SourceVersion: project.FormatVersion, SourcePath: "$.targetLoudnessDBQ8", InternalPath: "$.targetLoudnessDBQ8",
			Kind: TraceConvert, OldValue: strconv.FormatInt(old, 10), NewValue: strconv.FormatInt(change.TargetLoudnessDBQ8, 10),
		})
	case ChangeFrameRate:
		if err := ValidateFrameParameters(change.FrameLength, change.FrameShift); err != nil {
			return err
		}
		oldLength, oldShift := project.FrameLength, project.FrameShift
		project.FrameLength = change.FrameLength
		project.FrameShift = change.FrameShift
		project.Provenance.Traces = append(project.Provenance.Traces,
			FieldTrace{SourceVersion: project.FormatVersion, SourcePath: "$.frameLength", InternalPath: "$.frameLength", Kind: TraceConvert, OldValue: strconv.FormatInt(oldLength, 10), NewValue: strconv.FormatInt(change.FrameLength, 10)},
			FieldTrace{SourceVersion: project.FormatVersion, SourcePath: "$.frameShift", InternalPath: "$.frameShift", Kind: TraceConvert, OldValue: strconv.FormatInt(oldShift, 10), NewValue: strconv.FormatInt(change.FrameShift, 10)},
		)
	default:
		return &DomainError{Code: EPara, Path: "$.change.kind", Field: "kind", Detail: "unknown change kind"}
	}
	return nil
}

// MarkChange marks the actual downstream nodes invalid and returns merged
// clip/frame intervals. It does not mutate the project.
func (g *DependencyGraph) MarkChange(change Change) ([]Invalidation, error) {
	if g == nil {
		return nil, &DomainError{Code: ESchema, Path: "$.graph", Detail: "dependency graph is nil"}
	}
	var clipIDs []int
	var phase InvalidationPhase
	var reason string
	switch change.Kind {
	case ChangeGain:
		clipIDs = []int{change.ClipID}
		phase = PhaseEnvelope
		reason = "clip gain changed"
	case ChangeTarget:
		clipIDs = make([]int, 0, len(g.clipCal))
		for id := range g.clipCal {
			clipIDs = append(clipIDs, id)
		}
		sort.Ints(clipIDs)
		phase = PhaseCalibration
		reason = "target loudness changed"
	case ChangeFrameRate:
		clipIDs = make([]int, 0, len(g.clipCal))
		for id := range g.clipCal {
			clipIDs = append(clipIDs, id)
		}
		sort.Ints(clipIDs)
		phase = PhaseEnvelope
		reason = "frame length/shift changed"
	default:
		return nil, &DomainError{Code: EPara, Path: "$.change.kind", Field: "kind", Detail: "unknown change kind"}
	}
	invalidations := make([]Invalidation, 0)
	for _, clipID := range clipIDs {
		frameNodeKeys := g.frameNodeKeys(clipID)
		if len(frameNodeKeys) == 0 {
			return nil, &DomainError{Code: ESchema, Path: "$.clips", Field: "clipId", Detail: fmt.Sprintf("unknown clip %d in graph", clipID)}
		}
		switch change.Kind {
		case ChangeGain, ChangeFrameRate:
			for _, key := range frameNodeKeys {
				node := g.nodes[key]
				node.Valid = false
				invalidations = append(invalidations, Invalidation{
					ClipID: clipID, StartSample: node.StartSample, EndSample: node.EndSample,
					Phase: phase, Reason: reason,
				})
			}
			calKey := g.clipCal[clipID]
			if node := g.nodeByCacheKey(calKey); node != nil {
				node.Valid = false
			}
		case ChangeTarget:
			calNode := g.nodeByCacheKey(g.clipCal[clipID])
			if calNode != nil {
				calNode.Valid = false
			}
			for _, key := range frameNodeKeys {
				node := g.nodes[key]
				invalidations = append(invalidations, Invalidation{
					ClipID: clipID, StartSample: node.StartSample, EndSample: node.EndSample,
					Phase: phase, Reason: reason,
				})
			}
		}
	}
	return mergeInvalidations(invalidations), nil
}

func (g *DependencyGraph) frameNodeKeys(clipID int) []string {
	keys := make([]string, 0)
	prefix := "frame:" + strconv.Itoa(clipID) + ":"
	for key := range g.nodes {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return g.nodes[keys[i]].FrameIndex < g.nodes[keys[j]].FrameIndex
	})
	return keys
}

func (g *DependencyGraph) nodeByCacheKey(cacheKey string) *DependencyNode {
	for _, node := range g.nodes {
		if node.CacheKey == cacheKey {
			return node
		}
	}
	return nil
}

func mergeInvalidations(items []Invalidation) []Invalidation {
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ClipID != items[j].ClipID {
			return items[i].ClipID < items[j].ClipID
		}
		if items[i].Phase != items[j].Phase {
			return items[i].Phase < items[j].Phase
		}
		if items[i].StartSample != items[j].StartSample {
			return items[i].StartSample < items[j].StartSample
		}
		return items[i].EndSample < items[j].EndSample
	})
	merged := make([]Invalidation, 0, len(items))
	for _, item := range items {
		if len(merged) == 0 {
			merged = append(merged, item)
			continue
		}
		last := &merged[len(merged)-1]
		if last.ClipID == item.ClipID && last.Phase == item.Phase && item.StartSample <= last.EndSample {
			if item.EndSample > last.EndSample {
				last.EndSample = item.EndSample
			}
			if last.Reason != item.Reason {
				last.Reason += "; " + item.Reason
			}
			continue
		}
		merged = append(merged, item)
	}
	return merged
}

// Snapshot returns nodes sorted by clip, kind, and frame index.
func (g *DependencyGraph) Snapshot() DependencySnapshot {
	nodes := make([]DependencyNode, 0, len(g.nodes))
	for _, node := range g.nodes {
		copyNode := *node
		copyNode.Upstream = append([]string(nil), node.Upstream...)
		nodes = append(nodes, copyNode)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].ClipID != nodes[j].ClipID {
			return nodes[i].ClipID < nodes[j].ClipID
		}
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind < nodes[j].Kind
		}
		return nodes[i].FrameIndex < nodes[j].FrameIndex
	})
	return DependencySnapshot{Nodes: nodes}
}

// FrameCacheKeys returns per-frame cache keys for a clip.
func (g *DependencyGraph) FrameCacheKeys(clipID int) []string {
	keys := make([]string, 0)
	for _, nodeKey := range g.frameNodeKeys(clipID) {
		keys = append(keys, g.nodes[nodeKey].CacheKey)
	}
	return keys
}

// CalibrationCacheKey returns the cache key for a clip's calibration node.
func (g *DependencyGraph) CalibrationCacheKey(clipID int) string {
	return g.clipCal[clipID]
}

// ContentHash returns the current clip content hash used by graph nodes.
func (g *DependencyGraph) ContentHash(clipID int) string {
	key := "source:clip:" + strconv.Itoa(clipID) + ":"
	for nodeKey, node := range g.nodes {
		if node.Kind == NodeSource && len(nodeKey) >= len(key) && nodeKey[:len(key)] == key {
			return node.ContentHash
		}
	}
	return ""
}

// MeasurementCacheKey combines content, all frame interval keys, quant version,
// and upstream source key.
func (g *DependencyGraph) MeasurementCacheKey(clipID int) string {
	frameKeys := g.FrameCacheKeys(clipID)
	if len(frameKeys) == 0 {
		return ""
	}
	contentHash := g.ContentHash(clipID)
	return MakeCacheKey(contentHash, "measurement:"+strconv.Itoa(clipID), g.quantVersion, frameKeys)
}
