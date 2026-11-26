// Package graph builds the deterministic clip/frame dependency graph used for
// cache keys and precise invalidation.
package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"dialogueaudio/audioerr"
	"dialogueaudio/model"
)

// NodeKind identifies graph layers.
type NodeKind string

const (
	NodeInput       NodeKind = "input"
	NodeEnvelope    NodeKind = "envelope"
	NodeFrame       NodeKind = "frame_interval"
	NodeCalibration NodeKind = "calibration"
)

// Node is one cache-keyed graph node.
type Node struct {
	ID         string   `json:"id"`
	Kind       NodeKind `json:"kind"`
	ClipID     string   `json:"clip_id,omitempty"`
	FrameIndex int      `json:"frame_index,omitempty"`
	Interval   string   `json:"interval_id"`
	Key        string   `json:"cache_key"`
	KeyDigest  string   `json:"key_digest"`
	Upstream   []string `json:"upstream,omitempty"`
}

// Edge points from an upstream input/envelope to a downstream result.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Graph is immutable after Build.
type Graph struct {
	QuantVersion string              `json:"quantization_version"`
	Nodes        map[string]Node     `json:"nodes"`
	Order        []string            `json:"order"`
	Edges        []Edge              `json:"edges"`
	Out          map[string][]string `json:"-"`
}

// KeyFor is the single cache-key formula.
func KeyFor(contentHash, intervalID, quantVersion string, upstreamKeys []string) string {
	keys := append([]string(nil), upstreamKeys...)
	sort.Strings(keys)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "v1\ncontent=%s\ninterval=%s\nquant=%s\n", contentHash, intervalID, quantVersion)
	for _, key := range keys {
		_, _ = fmt.Fprintf(h, "upstream=%s\n", key)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Digest shortens a cache key for human-readable reports.
func Digest(key string) string {
	if len(key) <= 16 {
		return key
	}
	return key[:16]
}

// Build constructs all keys in dependency order.
func Build(project model.Project, quantVersion string) (*Graph, error) {
	g := &Graph{
		QuantVersion: quantVersion,
		Nodes:        make(map[string]Node),
		Out:          make(map[string][]string),
	}
	if quantVersion == "" {
		return nil, audioerr.Schema("quantization_version", "must not be empty")
	}
	addNode := func(node Node) {
		node.KeyDigest = Digest(node.Key)
		g.Nodes[node.ID] = node
		g.Order = append(g.Order, node.ID)
	}
	addEdge := func(from, to string) {
		g.Edges = append(g.Edges, Edge{From: from, To: to})
		g.Out[from] = append(g.Out[from], to)
	}
	addInput := func(id, interval string, values ...string) Node {
		payload := strings.Join(values, "|")
		key := KeyFor("", interval, quantVersion, []string{payload})
		node := Node{ID: id, Kind: NodeInput, Interval: interval, Key: key}
		addNode(node)
		return node
	}

	target := addInput("session:target", "session:target",
		fmt.Sprintf("target=%d", project.TargetLoudnessQ16))
	bound := addInput("session:bound", "session:bound",
		fmt.Sprintf("max_adjustment_db=%d", project.MaxAdjustmentDBQ16))
	fps := addInput("session:fps", "session:fps",
		fmt.Sprintf("rate=%d,frame=%d,hop=%d", project.SampleRate, project.FrameLenSamples, project.HopSamples))
	sessionGain := addInput("session:gain", "session:gain",
		fmt.Sprintf("session_gain_q16=%d", project.SessionGainQ16))
	calibrationFlag := addInput("session:calibration", "session:calibration",
		fmt.Sprintf("enabled=%t", project.CalibrationEnabled))

	clips := append([]model.Clip(nil), project.Clips...)
	sort.Slice(clips, func(i, j int) bool { return clips[i].ID < clips[j].ID })
	for _, clip := range clips {
		prefix := "clip:" + clip.ID
		content := addInput(prefix+":input:content", fmt.Sprintf("clip:%s:content", clip.ID), clip.ContentHash)
		boundary := addInput(prefix+":input:boundary", fmt.Sprintf("clip:%s:boundary", clip.ID),
			fmt.Sprintf("%d-%d", clip.StartSample, clip.EndSample))
		weight := addInput(prefix+":input:weight", fmt.Sprintf("clip:%s:weight", clip.ID),
			fmt.Sprintf("weight_q16=%d", clip.LoudnessWeightQ16))
		gain := addInput(prefix+":input:gain", fmt.Sprintf("clip:%s:gain", clip.ID),
			fmt.Sprintf("gain_q16=%d", clip.GainQ16))
		category := addInput(prefix+":input:category", fmt.Sprintf("clip:%s:category", clip.ID),
			clip.Category)

		envelopeUpstream := []string{
			g.Nodes[content.ID].Key,
			g.Nodes[boundary.ID].Key,
			g.Nodes[weight.ID].Key,
			g.Nodes[gain.ID].Key,
			g.Nodes[category.ID].Key,
			g.Nodes[fps.ID].Key,
			g.Nodes[sessionGain.ID].Key,
		}
		envelopeID := prefix + ":envelope"
		envelope := Node{
			ID: envelopeID, Kind: NodeEnvelope, ClipID: clip.ID,
			Interval: fmt.Sprintf("clip:%s:%d-%d", clip.ID, clip.StartSample, clip.EndSample),
			Upstream: upstreamIDs(content.ID, boundary.ID, weight.ID, gain.ID, category.ID, fps.ID, sessionGain.ID),
		}
		envelope.Key = KeyFor(clip.ContentHash, envelope.Interval, quantVersion, envelopeUpstream)
		addNode(envelope)
		addEdge(content.ID, envelopeID)
		addEdge(boundary.ID, envelopeID)
		addEdge(weight.ID, envelopeID)
		addEdge(gain.ID, envelopeID)
		addEdge(category.ID, envelopeID)
		addEdge(fps.ID, envelopeID)
		addEdge(sessionGain.ID, envelopeID)

		frameCount := clip.FrameCount(project.FrameLenSamples, project.HopSamples)
		frameKeys := make([]string, 0, frameCount)
		frameIDs := make([]string, 0, frameCount)
		for frameIndex := 0; frameIndex < frameCount; frameIndex++ {
			start := clip.StartSample + int64(frameIndex)*project.HopSamples
			end := start + project.FrameLenSamples
			if end > clip.EndSample {
				end = clip.EndSample
			}
			frameID := fmt.Sprintf("%s:frame:%06d", prefix, frameIndex)
			frame := Node{
				ID: frameID, Kind: NodeFrame, ClipID: clip.ID, FrameIndex: frameIndex,
				Interval: fmt.Sprintf("frame:%s:%06d:%d-%d", clip.ID, frameIndex, start, end),
				Upstream: []string{envelopeID},
			}
			frame.Key = KeyFor(clip.ContentHash, frame.Interval, quantVersion, []string{envelope.Key})
			addNode(frame)
			addEdge(envelopeID, frameID)
			frameKeys = append(frameKeys, frame.Key)
			frameIDs = append(frameIDs, frameID)
		}
		calibrationID := prefix + ":calibration"
		calibrationUpstream := append([]string{
			g.Nodes[envelope.ID].Key, g.Nodes[target.ID].Key, g.Nodes[bound.ID].Key,
			g.Nodes[gain.ID].Key, g.Nodes[calibrationFlag.ID].Key,
		}, frameKeys...)
		calibration := Node{
			ID: calibrationID, Kind: NodeCalibration, ClipID: clip.ID,
			Interval: fmt.Sprintf("calibration:%s", clip.ID),
			Upstream: append(upstreamIDs(envelopeID, target.ID, bound.ID, gain.ID, calibrationFlag.ID), frameIDs...),
		}
		calibration.Key = KeyFor(clip.ContentHash, calibration.Interval, quantVersion, calibrationUpstream)
		addNode(calibration)
		addEdge(envelopeID, calibrationID)
		addEdge(target.ID, calibrationID)
		addEdge(bound.ID, calibrationID)
		addEdge(gain.ID, calibrationID)
		addEdge(calibrationFlag.ID, calibrationID)
		for _, frameID := range frameIDs {
			addEdge(frameID, calibrationID)
		}
	}
	for id := range g.Out {
		sort.Strings(g.Out[id])
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		return g.Edges[i].To < g.Edges[j].To
	})
	return g, nil
}

func upstreamIDs(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// EditKind is an input mutation.
type EditKind string

const (
	EditClipGain       EditKind = "clip_gain"
	EditTarget         EditKind = "target_loudness"
	EditFrameRate      EditKind = "frame_rate"
	EditClipBoundary   EditKind = "clip_boundary"
	EditLoudnessWeight EditKind = "loudness_weight"
	EditContent        EditKind = "clip_content"
	EditSessionGain    EditKind = "session_gain"
	EditBound          EditKind = "adjustment_bound"
	EditCalibration    EditKind = "calibration_enabled"
	EditQuantization   EditKind = "quantization_version"
)

// Edit identifies the changed input. ClipID is ignored for session edits.
type Edit struct {
	Kind   EditKind `json:"kind"`
	ClipID string   `json:"clip_id,omitempty"`
}

// FrameInvalidation lists the actual affected frame intervals of one clip.
type FrameInvalidation struct {
	ClipID       string   `json:"clip_id"`
	FrameIndices []int    `json:"frame_indices"`
	KeyDigests   []string `json:"key_digests"`
}

// NodeRef is a compact affected-node record.
type NodeRef struct {
	ID        string   `json:"id"`
	Kind      NodeKind `json:"kind"`
	ClipID    string   `json:"clip_id,omitempty"`
	KeyDigest string   `json:"key_digest"`
}

// Invalidation is the precise invalidation report.
type Invalidation struct {
	Edit                Edit                `json:"edit"`
	InvalidatedNodes    []NodeRef           `json:"invalidated_nodes"`
	AffectedClips       []string            `json:"affected_clips"`
	FrameIntervals      []FrameInvalidation `json:"frame_intervals"`
	ReusedEnvelopeClips []string            `json:"reused_envelope_clips"`
}

// Invalidate walks descendants of the edited input and reports only the
// actually affected cache nodes.
func (g *Graph) Invalidate(edit Edit) (Invalidation, error) {
	starts := []string{}
	switch edit.Kind {
	case EditClipGain, EditClipBoundary, EditLoudnessWeight, EditContent:
		if edit.ClipID == "" {
			return Invalidation{}, audioerr.Schema("edit.clip_id", "required for clip edits")
		}
		base := "clip:" + edit.ClipID
		switch edit.Kind {
		case EditClipGain:
			starts = append(starts, base+":input:gain")
		case EditClipBoundary:
			starts = append(starts, base+":input:boundary")
		case EditLoudnessWeight:
			starts = append(starts, base+":input:weight")
		case EditContent:
			starts = append(starts, base+":input:content")
		}
	case EditTarget:
		starts = append(starts, "session:target")
	case EditFrameRate:
		starts = append(starts, "session:fps")
	case EditSessionGain:
		starts = append(starts, "session:gain")
	case EditBound:
		starts = append(starts, "session:bound")
	case EditCalibration:
		starts = append(starts, "session:calibration")
	case EditQuantization:
		for id := range g.Nodes {
			starts = append(starts, id)
		}
	default:
		return Invalidation{}, audioerr.Schema("edit.kind", "unknown edit kind "+string(edit.Kind))
	}
	visited := make(map[string]bool)
	queue := append([]string(nil), starts...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		if _, ok := g.Nodes[id]; !ok {
			return Invalidation{}, audioerr.Schema("graph."+id, "node does not exist")
		}
		visited[id] = true
		queue = append(queue, g.Out[id]...)
	}
	report := Invalidation{
		Edit: edit, InvalidatedNodes: []NodeRef{}, AffectedClips: []string{},
		FrameIntervals: []FrameInvalidation{}, ReusedEnvelopeClips: []string{},
	}
	affectedClips := make(map[string]bool)
	envelopeAffected := make(map[string]bool)
	frames := make(map[string][]Node)
	for id := range visited {
		node := g.Nodes[id]
		report.InvalidatedNodes = append(report.InvalidatedNodes, NodeRef{
			ID: node.ID, Kind: node.Kind, ClipID: node.ClipID, KeyDigest: node.KeyDigest,
		})
		if node.ClipID != "" {
			affectedClips[node.ClipID] = true
		}
		switch node.Kind {
		case NodeEnvelope:
			envelopeAffected[node.ClipID] = true
		case NodeFrame:
			frames[node.ClipID] = append(frames[node.ClipID], node)
		}
	}
	for clipID := range affectedClips {
		report.AffectedClips = append(report.AffectedClips, clipID)
	}
	sort.Strings(report.AffectedClips)
	for clipID, frameNodes := range frames {
		sort.Slice(frameNodes, func(i, j int) bool { return frameNodes[i].FrameIndex < frameNodes[j].FrameIndex })
		fi := FrameInvalidation{ClipID: clipID}
		for _, frame := range frameNodes {
			fi.FrameIndices = append(fi.FrameIndices, frame.FrameIndex)
			fi.KeyDigests = append(fi.KeyDigests, frame.KeyDigest)
		}
		report.FrameIntervals = append(report.FrameIntervals, fi)
	}
	sort.Slice(report.FrameIntervals, func(i, j int) bool {
		return report.FrameIntervals[i].ClipID < report.FrameIntervals[j].ClipID
	})
	sort.Slice(report.InvalidatedNodes, func(i, j int) bool {
		return report.InvalidatedNodes[i].ID < report.InvalidatedNodes[j].ID
	})
	clipIDs := make([]string, 0, len(g.Nodes))
	seen := make(map[string]bool)
	for _, id := range g.Order {
		clipID := g.Nodes[id].ClipID
		if clipID != "" && !seen[clipID] {
			seen[clipID] = true
			clipIDs = append(clipIDs, clipID)
		}
	}
	for _, clipID := range clipIDs {
		if !envelopeAffected[clipID] {
			report.ReusedEnvelopeClips = append(report.ReusedEnvelopeClips, clipID)
		}
	}
	return report, nil
}

// Node returns a node by ID.
func (g *Graph) Node(id string) (Node, bool) {
	node, ok := g.Nodes[id]
	return node, ok
}
