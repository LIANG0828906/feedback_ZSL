package dialogue

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

type NodeKind string

const (
	NodeProjectTarget   NodeKind = "project-target"
	NodeProjectFrame    NodeKind = "project-frame-config"
	NodeSegmentSignal   NodeKind = "segment-signal"
	NodeFrameRange      NodeKind = "frame-range"
	NodeSegmentEnvelope NodeKind = "segment-envelope"
	NodeCalibration     NodeKind = "calibration"
)

type DependencyNode struct {
	ID         string            `json:"id"`
	Kind       NodeKind          `json:"kind"`
	SegmentID  string            `json:"segment_id,omitempty"`
	FrameIndex *int              `json:"frame_index,omitempty"`
	Key        string            `json:"key"`
	Upstream   []string          `json:"upstream"`
	Inputs     map[string]string `json:"inputs,omitempty"`
}

type InvalidationPlan struct {
	Reason       string   `json:"reason"`
	Invalidated  []string `json:"invalidated"`
	RebuildOrder []string `json:"rebuild_order"`
	Reused       []string `json:"reused"`
	SegmentIDs   []string `json:"segment_ids"`
}

type DependencyGraph struct {
	Nodes map[string]*DependencyNode `json:"nodes"`
	Order []string                   `json:"order"`
	byID  map[string][]string
}

func NewDependencyGraph(project *Project) *DependencyGraph {
	graph := &DependencyGraph{Nodes: map[string]*DependencyNode{}, byID: map[string][]string{}}
	targetID := "project/target"
	graph.addNode(&DependencyNode{
		ID: targetID, Kind: NodeProjectTarget,
		Key:    cacheKey("target", fmt.Sprintf("%d", project.TargetLoudness), QuantizationVersion, AlgorithmVersion),
		Inputs: map[string]string{"target_loudness": fmt.Sprintf("%d", project.TargetLoudness)},
	})
	frameID := "project/frame-config"
	graph.addNode(&DependencyNode{
		ID: frameID, Kind: NodeProjectFrame,
		Key: cacheKey("frame-config", fmt.Sprintf("%d:%d:%d", project.SampleRate, project.FrameLength, project.Hop), QuantizationVersion, AlgorithmVersion),
		Inputs: map[string]string{
			"sample_rate":  fmt.Sprintf("%d", project.SampleRate),
			"frame_length": fmt.Sprintf("%d", project.FrameLength),
			"hop":          fmt.Sprintf("%d", project.Hop),
		},
	})
	for _, segment := range project.Segments() {
		signalID := segmentNodeID(segment.ID, "signal")
		signalHash := project.SignalHash(segment)
		graph.addNode(&DependencyNode{
			ID: signalID, Kind: NodeSegmentSignal, SegmentID: segment.ID,
			Key:      cacheKey("signal", signalHash, QuantizationVersion, AlgorithmVersion, graph.Nodes[frameID].Key),
			Upstream: []string{frameID},
			Inputs: map[string]string{
				"start_sample": fmt.Sprintf("%d", segment.StartSample),
				"end_sample":   fmt.Sprintf("%d", segment.EndSample),
				"weights":      weightSignature(segment),
			},
		})
		count := frameCount(segment, project.FrameLength, project.Hop)
		frameIDs := make([]string, 0, count)
		for index := 0; index < count; index++ {
			start := segment.StartSample + int64(index)*project.Hop
			end := start + project.FrameLength
			id := fmt.Sprintf("seg.%s.frame.%04d", segment.ID, index)
			graph.addNode(&DependencyNode{
				ID: id, Kind: NodeFrameRange, SegmentID: segment.ID, FrameIndex: intPointer(index),
				Key:      cacheKey("frame", signalHash, fmt.Sprintf("%s:%d-%d", segment.ID, start, end), QuantizationVersion, AlgorithmVersion, graph.Nodes[signalID].Key),
				Upstream: []string{signalID},
				Inputs: map[string]string{
					"interval":    fmt.Sprintf("%s:%d-%d", segment.ID, start, end),
					"signal_hash": signalHash,
				},
			})
			frameIDs = append(frameIDs, id)
		}
		envelopeID := segmentNodeID(segment.ID, "envelope")
		upstreamKeys := []string{signalHash}
		for _, id := range frameIDs {
			upstreamKeys = append(upstreamKeys, graph.Nodes[id].Key)
		}
		graph.addNode(&DependencyNode{
			ID: envelopeID, Kind: NodeSegmentEnvelope, SegmentID: segment.ID,
			Key:      cacheKey(append([]string{"envelope", fmt.Sprintf("%d", len(frameIDs)), QuantizationVersion, AlgorithmVersion}, upstreamKeys...)...),
			Upstream: frameIDs,
			Inputs: map[string]string{
				"frame_count":   fmt.Sprintf("%d", len(frameIDs)),
				"normalization": "segment-max",
			},
		})
		calibrationID := segmentNodeID(segment.ID, "calibration")
		graph.addNode(&DependencyNode{
			ID: calibrationID, Kind: NodeCalibration, SegmentID: segment.ID,
			Key: cacheKey("calibration", project.ContentHash(segment), graph.Nodes[envelopeID].Key, graph.Nodes[targetID].Key,
				fmt.Sprintf("%d:%d:%d", segment.SessionGain, project.GainLimit, project.GainMin), fmt.Sprintf("%d", project.GainMax), QuantizationVersion, AlgorithmVersion),
			Upstream: []string{envelopeID, targetID, signalID},
			Inputs: map[string]string{
				"session_gain": fmt.Sprintf("%d", segment.SessionGain),
				"gain_limit":   fmt.Sprintf("%d", project.GainLimit),
				"gain_scale":   fmt.Sprintf("%d", InternalScale),
				"content_hash": project.ContentHash(segment),
			},
		})
	}
	return graph
}

func (g *DependencyGraph) addNode(node *DependencyNode) {
	if _, exists := g.Nodes[node.ID]; exists {
		return
	}
	g.Nodes[node.ID] = node
	g.Order = append(g.Order, node.ID)
	for _, upstream := range node.Upstream {
		g.byID[upstream] = append(g.byID[upstream], node.ID)
	}
}

func (g *DependencyGraph) PlanForEdit(edit Edit) *InvalidationPlan {
	var roots []string
	switch edit.Kind {
	case EditTargetLoudness, EditGainLimit:
		for _, id := range g.Order {
			if g.Nodes[id].Kind == NodeCalibration {
				roots = append(roots, id)
			}
		}
	case EditSegmentGain:
		roots = append(roots, segmentNodeID(edit.SegmentID, "calibration"))
	case EditSegmentContent:
		roots = append(roots, segmentNodeID(edit.SegmentID, "signal"))
	case EditFrameRate:
		for _, id := range g.Order {
			if g.Nodes[id].Kind == NodeSegmentSignal || g.Nodes[id].Kind == NodeFrameRange ||
				g.Nodes[id].Kind == NodeSegmentEnvelope || g.Nodes[id].Kind == NodeCalibration ||
				g.Nodes[id].Kind == NodeProjectFrame {
				roots = append(roots, id)
			}
		}
	default:
		roots = nil
	}
	return g.invalidate(roots, string(edit.Kind))
}

func (g *DependencyGraph) invalidate(roots []string, reason string) *InvalidationPlan {
	invalidSet := map[string]bool{}
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if invalidSet[id] {
			continue
		}
		if _, ok := g.Nodes[id]; !ok {
			continue
		}
		invalidSet[id] = true
		queue = append(queue, g.byID[id]...)
	}
	plan := &InvalidationPlan{Reason: reason}
	for _, id := range g.Order {
		if invalidSet[id] {
			plan.Invalidated = append(plan.Invalidated, id)
			plan.RebuildOrder = append(plan.RebuildOrder, id)
		} else {
			plan.Reused = append(plan.Reused, id)
		}
	}
	seen := map[string]bool{}
	for _, id := range plan.Invalidated {
		segmentID := g.Nodes[id].SegmentID
		if segmentID != "" && !seen[segmentID] {
			seen[segmentID] = true
			plan.SegmentIDs = append(plan.SegmentIDs, segmentID)
		}
	}
	return plan
}

func (g *DependencyGraph) NodeIDs(kind NodeKind) []string {
	var ids []string
	for _, id := range g.Order {
		if g.Nodes[id].Kind == kind {
			ids = append(ids, id)
		}
	}
	return ids
}

func segmentNodeID(segmentID, suffix string) string {
	return "seg." + segmentID + "." + suffix
}

func cacheKey(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		writeString(h, part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func intPointer(value int) *int {
	return &value
}

func weightSignature(segment Segment) string {
	signature := ""
	for _, phoneme := range segment.Phonemes {
		signature += fmt.Sprintf("%s:%s:%d;", phoneme.ID, phoneme.Kind, phoneme.Weight)
	}
	return signature
}

type EditKind string

const (
	EditSegmentGain    EditKind = "segment-gain"
	EditTargetLoudness EditKind = "target-loudness"
	EditGainLimit      EditKind = "gain-limit"
	EditFrameRate      EditKind = "frame-rate"
	EditSegmentContent EditKind = "segment-content"
)

type Edit struct {
	Kind        EditKind `json:"kind"`
	SegmentID   string   `json:"segment_id,omitempty"`
	Value       int64    `json:"value,omitempty"`
	FrameLength int64    `json:"frame_length,omitempty"`
	Hop         int64    `json:"hop,omitempty"`
	PCM         []int16  `json:"pcm,omitempty"`
	ReplacePCM  bool     `json:"replace_pcm,omitempty"`
}
