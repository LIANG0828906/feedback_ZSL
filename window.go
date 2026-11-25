package vpcompose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

type WindowState string

const (
	WindowPending     WindowState = "pending"
	WindowCompositing WindowState = "compositing"
	WindowCommitted   WindowState = "committed"
	WindowFailed      WindowState = "failed"
)

func (s WindowState) Valid() bool {
	switch s {
	case WindowPending, WindowCompositing, WindowCommitted, WindowFailed:
		return true
	default:
		return false
	}
}

type StackSnapshot struct {
	WindowIndex      int           `json:"windowIndex"`
	ProjectID        string        `json:"projectId"`
	Width            int           `json:"width"`
	Height           int           `json:"height"`
	Channels         int           `json:"channels"`
	Timebase         Timebase      `json:"timebase"`
	SourceVersion    Version       `json:"sourceVersion"`
	ParamVersion     uint64        `json:"paramVersion"`
	ContentHash      string        `json:"contentHash"`
	AlgorithmVersion string        `json:"algorithmVersion"`
	Layers           []*Layer      `json:"layers"`
	Instructions     []Instruction `json:"instructions"`
	CacheKey         string        `json:"cacheKey"`
}

func CaptureStackSnapshot(project *Project) (*StackSnapshot, error) {
	return CaptureWindowSnapshot(project, -1)
}

func CaptureWindowSnapshot(project *Project, windowIndex int) (*StackSnapshot, error) {
	if project == nil {
		return nil, schemaError("project", "nil project")
	}
	snapshot := &StackSnapshot{
		WindowIndex: windowIndex,
		ProjectID:   project.ID, Width: project.Width, Height: project.Height,
		Channels: project.Channels, Timebase: project.Timebase,
		SourceVersion: project.SourceVersion, ParamVersion: project.ParamVersion,
		ContentHash: project.ContentHash(), AlgorithmVersion: project.AlgorithmVersion,
		Instructions: append([]Instruction(nil), project.Instructions...),
	}
	for _, layer := range project.Layers {
		copyLayer := *layer
		snapshot.Layers = append(snapshot.Layers, &copyLayer)
	}
	var relevant map[string]bool
	if windowIndex >= 0 {
		if start, end, ok := project.WindowBounds(windowIndex); ok {
			relevant = make(map[string]bool)
			for _, layer := range project.Layers {
				if layerContributesToWindow(project, layer.ID, start, end) {
					relevant[layer.ID] = true
				}
			}
		}
	}
	snapshot.CacheKey = stackCacheKey(snapshot, relevant)
	return snapshot, nil
}

func stackCacheKey(snapshot *StackSnapshot, relevant map[string]bool) string {
	upstream := fmt.Sprintf("window:%d:%s:%s", snapshot.WindowIndex, snapshot.ProjectID, snapshot.SourceVersion.String())
	for _, layer := range snapshot.Layers {
		if relevant != nil && !relevant[layer.ID] {
			continue
		}
		h := sha256.New()
		h.Write([]byte(upstream))
		h.Write([]byte{0})
		h.Write([]byte(layer.ID))
		h.Write([]byte{0})
		h.Write([]byte(layer.ContentHash))
		h.Write([]byte{0})
		_ = json.NewEncoder(h).Encode(layer.ParamVersion)
		upstream = hex.EncodeToString(h.Sum(nil))
	}
	h := sha256.New()
	h.Write([]byte(upstream))
	h.Write([]byte{0})
	_ = json.NewEncoder(h).Encode(snapshot.Timebase)
	h.Write([]byte(snapshot.AlgorithmVersion))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *StackSnapshot) Digest() string {
	if s == nil {
		return ""
	}
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *StackSnapshot) LayerPointers() []*Layer {
	if s == nil {
		return nil
	}
	out := make([]*Layer, len(s.Layers))
	copy(out, s.Layers)
	return out
}

type SampleStats struct {
	FrameIndex  int       `json:"frameIndex"`
	TimeTicks   int64     `json:"timeTick"`
	SampleCount int       `json:"sampleCount"`
	Sum         [4]uint64 `json:"sum"`
	Digest      string    `json:"digest"`
}

type WindowResult struct {
	WindowIndex  int           `json:"windowIndex"`
	Frames       []*Frame      `json:"frames"`
	FrameDigests []string      `json:"frameDigests"`
	SampleStats  []SampleStats `json:"sampleStats"`
	Digest       string        `json:"digest"`
}

func BuildWindowResult(project *Project, snapshot *StackSnapshot, windowIndex int) (*WindowResult, error) {
	start, end, ok := project.WindowBounds(windowIndex)
	if !ok {
		return nil, schemaError("windowIndex", "outside project windows")
	}
	result := &WindowResult{WindowIndex: windowIndex}
	h := sha256.New()
	for i := start; i < end; i++ {
		frame, err := ComposeFrameWithLayers(project, project.Frames[i], snapshot.LayerPointers(), snapshot.Instructions)
		if err != nil {
			return nil, err
		}
		digest := frame.Digest()
		result.Frames = append(result.Frames, frame)
		result.FrameDigests = append(result.FrameDigests, digest)
		stats := sampleStats(project.Frames[i], frame)
		result.SampleStats = append(result.SampleStats, stats)
		h.Write([]byte(digest))
		h.Write([]byte{0})
		h.Write([]byte(stats.Digest))
		h.Write([]byte{0})
	}
	result.Digest = hex.EncodeToString(h.Sum(nil))
	return result, nil
}

func sampleStats(spec FrameSpec, frame *Frame) SampleStats {
	stats := SampleStats{FrameIndex: spec.Index, TimeTicks: spec.TimeTicks, SampleCount: len(spec.SamplePoints)}
	digest := sha256.New()
	for _, point := range spec.SamplePoints {
		pixel, ok := frame.Pixel(point.X, point.Y)
		if !ok {
			continue
		}
		for i := 0; i < 4; i++ {
			stats.Sum[i] += uint64(pixel[i])
		}
		digest.Write([]byte{byte(point.X), byte(point.X >> 8), byte(point.Y), byte(point.Y >> 8)})
		for _, value := range pixel {
			digest.Write([]byte{byte(value), byte(value >> 8)})
		}
	}
	stats.Digest = hex.EncodeToString(digest.Sum(nil))
	return stats
}

type QueueEvent struct {
	Window int           `json:"window"`
	Reason string        `json:"reason"`
	Wait   time.Duration `json:"waitNanos"`
	Worker int           `json:"worker"`
}

type Window struct {
	Index       int            `json:"index"`
	FrameStart  int            `json:"frameStart"`
	FrameEnd    int            `json:"frameEnd"`
	State       WindowState    `json:"state"`
	Snapshot    *StackSnapshot `json:"snapshot,omitempty"`
	Result      *WindowResult  `json:"result,omitempty"`
	LastError   string         `json:"lastError,omitempty"`
	CommitOrder int            `json:"commitOrder,omitempty"`
}

func NewWindow(index int, project *Project) *Window {
	start, end, _ := project.WindowBounds(index)
	return &Window{Index: index, FrameStart: start, FrameEnd: end, State: WindowPending}
}

func (w *Window) transition(target WindowState) error {
	legal := false
	switch w.State {
	case WindowPending:
		legal = target == WindowCompositing
	case WindowCompositing:
		legal = target == WindowCommitted || target == WindowFailed
	case WindowFailed:
		legal = target == WindowCompositing
	case WindowCommitted:
		legal = false
	}
	if !legal {
		return &EngineError{
			Code: CodeState, Window: w.Index,
			Message:  fmt.Sprintf("illegal window transition %s -> %s", w.State, target),
			Expected: w.State, Actual: target,
		}
	}
	w.State = target
	return nil
}

func (w *Window) Start() error {
	if err := w.transition(WindowCompositing); err != nil {
		return err
	}
	w.Result = nil
	w.LastError = ""
	return nil
}

func (w *Window) Commit(result *WindowResult) error {
	if result == nil {
		return schemaError("window.result", "nil result")
	}
	if err := w.transition(WindowCommitted); err != nil {
		return err
	}
	w.Result = result
	w.CommitOrder = w.Index
	return nil
}

func (w *Window) Fail(err error) error {
	if err := w.transition(WindowFailed); err != nil {
		return err
	}
	if err != nil {
		w.LastError = err.Error()
	}
	w.Result = nil
	return nil
}

type WindowProgress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

func Progress(windows []*Window) WindowProgress {
	progress := WindowProgress{Total: len(windows)}
	for _, window := range windows {
		if window.State == WindowCommitted {
			progress.Committed++
		}
	}
	return progress
}

type DependencyGraph struct {
	LayerKeys  map[string]map[string]string `json:"layerKeys"`
	WindowKeys map[int]string               `json:"windowKeys"`
	ExportKey  string                       `json:"exportKey"`
}

func BuildDependencies(project *Project) *DependencyGraph {
	graph := &DependencyGraph{
		LayerKeys:  make(map[string]map[string]string),
		WindowKeys: make(map[int]string),
	}
	exportH := sha256.New()
	for index := 0; index < project.WindowCount(); index++ {
		start, end, _ := project.WindowBounds(index)
		upstream := fmt.Sprintf("window:%d:%s:%s", index, project.ID, project.SourceVersion.String())
		perLayer := make(map[string]string)
		for _, layer := range project.Layers {
			if !layerContributesToWindow(project, layer.ID, start, end) {
				continue
			}
			key := layerDependencyKey(upstream, layer)
			perLayer[layer.ID] = key
			upstream = key
		}
		key := fmt.Sprintf("%s:%d:%d", upstream, start, end)
		graph.LayerKeys[fmt.Sprintf("window:%d", index)] = perLayer
		graph.WindowKeys[index] = key
		exportH.Write([]byte(key))
		exportH.Write([]byte{0})
	}
	graph.ExportKey = hex.EncodeToString(exportH.Sum(nil))
	return graph
}

func layerContributesToWindow(project *Project, layerID string, start, end int) bool {
	for frameIndex := start; frameIndex < end; frameIndex++ {
		frame := project.Frames[frameIndex]
		states, err := layersAtFrame(project.Layers, project.Instructions, frame.TimeTicks)
		if err != nil {
			return true
		}
		state := findLayerState(states, layerID)
		if state != nil && layerVisible(*state, frame.TimeTicks) {
			return true
		}
	}
	return false
}

func layerDependencyKey(upstream string, layer *Layer) string {
	h := sha256.New()
	h.Write([]byte(upstream))
	h.Write([]byte{0})
	h.Write([]byte(layer.ID))
	h.Write([]byte{0})
	h.Write([]byte(layer.ContentHash))
	h.Write([]byte{0})
	_ = json.NewEncoder(h).Encode(layer)
	return hex.EncodeToString(h.Sum(nil))
}

type InvalidationEntry struct {
	Window     int      `json:"window"`
	LayerIDs   []string `json:"layerIds"`
	Reason     string   `json:"reason"`
	Downstream []string `json:"downstream"`
}

type InvalidationReport struct {
	Entries []InvalidationEntry `json:"entries"`
}

func (r *InvalidationReport) WindowIDs() []int {
	ids := make([]int, 0, len(r.Entries))
	for _, entry := range r.Entries {
		ids = append(ids, entry.Window)
	}
	sort.Ints(ids)
	return ids
}
