// Package workbench implements the dependency graph, incremental cache,
// versioned SQLite snapshots, resumable execution, and diagnostics.
package workbench

import (
	"fmt"
	"time"

	"yuvench/filters"
	"yuvench/yuv"
)

const (
	SchemaVersion    = yuv.SchemaVersion
	AlgorithmVersion = "filter-chain-v1"
)

type Node struct {
	Seq           int      `json:"seq"`
	ID            string   `json:"id"`
	Plane         string   `json:"plane"`
	Stage         string   `json:"stage"`
	Deps          []string `json:"dependencies"`
	InputHash     string   `json:"inputHash"`
	ParamsHash    string   `json:"paramsHash"`
	ParamsJSON    []byte   `json:"-"`
	CacheKey      string   `json:"cacheKey"`
	ChromaVersion string   `json:"chromaVersion"`
}

type Graph struct {
	Nodes         []*Node          `json:"nodes"`
	ByID          map[string]*Node `json:"-"`
	FinalID       string           `json:"finalNode"`
	InputHash     string           `json:"inputHash"`
	ParamVersion  string           `json:"paramVersion"`
	ChromaVersion string           `json:"chromaVersion"`
}

type NodeOutput struct {
	Plane     *yuv.Plane     `json:"-"`
	Composite *yuv.Composite `json:"-"`
	Summary   string         `json:"summary"`
}

func (o *NodeOutput) Clone() *NodeOutput {
	if o == nil {
		return nil
	}
	return &NodeOutput{
		Plane:     o.Plane.Clone(),
		Composite: o.Composite.Clone(),
		Summary:   o.Summary,
	}
}

type CacheEntry struct {
	NodeID        string      `json:"nodeId"`
	CacheKey      string      `json:"cacheKey"`
	InputHash     string      `json:"inputHash"`
	ParamsHash    string      `json:"paramsHash"`
	ParamVersion  string      `json:"paramVersion"`
	ChromaVersion string      `json:"chromaVersion"`
	UpstreamKeys  []string    `json:"upstreamKeys"`
	Summary       string      `json:"summary"`
	Output        *NodeOutput `json:"-"`
}

type Invalidation struct {
	Plane  string `json:"plane"`
	Stage  string `json:"stage"`
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

type SnapshotMeta struct {
	RunID            string `json:"runId"`
	SchemaVersion    string `json:"schemaVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	ParamVersion     string `json:"paramVersion"`
	ChromaVersion    string `json:"chromaVersion"`
	InputHash        string `json:"inputHash"`
	ManifestHash     string `json:"manifestHash"`
	Complete         bool   `json:"complete"`
}

type StageCommit struct {
	Seq      int    `json:"seq"`
	NodeID   string `json:"nodeId"`
	Plane    string `json:"plane"`
	Stage    string `json:"stage"`
	CacheKey string `json:"cacheKey"`
	Summary  string `json:"summary"`
	Reused   bool   `json:"reused"`
}

type StageStat struct {
	NodeID        string  `json:"nodeId"`
	Plane         string  `json:"plane"`
	Stage         string  `json:"stage"`
	Seq           int     `json:"seq"`
	DurationMS    float64 `json:"durationMs"`
	Recomputed    bool    `json:"recomputed"`
	CacheHit      bool    `json:"cacheHit"`
	Committed     bool    `json:"committed"`
	Persisted     bool    `json:"persisted"`
	Reused        bool    `json:"reused,omitempty"`
	Failed        bool    `json:"failed"`
	Error         string  `json:"error,omitempty"`
	InvalidReason string  `json:"invalidReason,omitempty"`
}

type Stats struct {
	Mode            string      `json:"mode"`
	Recomputed      int         `json:"recomputed"`
	CacheHits       int         `json:"cacheHits"`
	Committed       int         `json:"committed"`
	ReusedCommitted int         `json:"reusedCommitted"`
	Failed          int         `json:"failed"`
	BufferWaitCount int         `json:"bufferWaitCount"`
	BufferWaits     []string    `json:"bufferWaits,omitempty"`
	TotalDurationMS float64     `json:"totalDurationMs"`
	Stages          []StageStat `json:"stages"`
}

type RunOptions struct {
	Mode              string
	ForceFull         bool
	PauseAfterCommits int
	Pause             <-chan struct{}
	Fault             func(node string) error
}

type RunResult struct {
	OK              bool                   `json:"ok"`
	Mode            string                 `json:"mode"`
	RunID           string                 `json:"runId"`
	ImageName       string                 `json:"imageName"`
	Image           yuv.ImageMeta          `json:"image"`
	Params          filters.Params         `json:"params"`
	ParamsVersion   string                 `json:"paramsVersion"`
	InputHash       string                 `json:"inputHash"`
	OriginalSummary string                 `json:"originalSummary"`
	FinalSummary    string                 `json:"finalSummary"`
	FinalSize       [2]int                 `json:"finalSize"`
	Complete        bool                   `json:"complete"`
	Paused          bool                   `json:"paused"`
	Invalidations   []Invalidation         `json:"invalidations"`
	Graph           *Graph                 `json:"graph,omitempty"`
	CommitSequence  []StageCommit          `json:"commitSequence"`
	Stats           Stats                  `json:"stats"`
	Snapshot        SnapshotMeta           `json:"snapshot"`
	Original        *yuv.Composite         `json:"-"`
	Final           *yuv.Composite         `json:"-"`
	Nodes           map[string]*NodeOutput `json:"-"`
	SnapshotReason  string                 `json:"snapshotInvalidation,omitempty"`
	Heatmap         *HeatmapStats          `json:"heatmap,omitempty"`
}

func (r *RunResult) NodeOutputs() map[string]*NodeOutput {
	if r == nil {
		return nil
	}
	return r.Nodes
}

func (r *RunResult) StageByNode(node string) *StageStat {
	if r == nil {
		return nil
	}
	for i := range r.Stats.Stages {
		if r.Stats.Stages[i].NodeID == node {
			return &r.Stats.Stages[i]
		}
	}
	return nil
}

type HeatmapStats struct {
	MaxDifference  float64 `json:"maxDifference"`
	MeanDifference float64 `json:"meanDifference"`
	ChangedPixels  int     `json:"changedPixels"`
	Normalization  float64 `json:"normalization"`
	Summary        string  `json:"summary,omitempty"`
}

type CompareMismatch struct {
	Code         string `json:"code"`
	NodeID       string `json:"nodeId"`
	Plane        string `json:"plane"`
	Stage        string `json:"stage"`
	X            int    `json:"x"`
	Y            int    `json:"y"`
	Left         uint16 `json:"left"`
	Right        uint16 `json:"right"`
	LeftSummary  string `json:"leftSummary,omitempty"`
	RightSummary string `json:"rightSummary,omitempty"`
}

func (m *CompareMismatch) Error() string {
	return fmt.Sprintf("%s: node=%s plane=%s stage=%s pixel=(%d,%d) left=%d right=%d", m.Code, m.NodeID, m.Plane, m.Stage, m.X, m.Y, m.Left, m.Right)
}

type Timing struct {
	Start time.Time
}

func NowMS(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}
