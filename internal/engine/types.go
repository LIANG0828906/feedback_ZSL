package engine

import (
	"sync"

	"maskpipeline/internal/checkpoint"
	"maskpipeline/internal/model"
	"maskpipeline/internal/sample"
	"maskpipeline/internal/version"
)

type ExecutorOptions struct {
	MaxParallel            int
	PerBlockBuffer         int
	MaxStageBytes          int
	PreviewIterationBudget int
	MaxAreaRatio           float64
	OutsideTruthRatio      float64
	InjectBadTile          int
	InjectBadStage         model.Stage
}

func DefaultExecutorOptions() ExecutorOptions {
	return ExecutorOptions{
		MaxParallel:            4,
		PerBlockBuffer:         2,
		MaxStageBytes:          1 << 20,
		PreviewIterationBudget: 8192,
		MaxAreaRatio:           model.DefaultSpillAreaRate,
		OutsideTruthRatio:      0.25,
		InjectBadTile:          -1,
	}
}

type Engine struct {
	Store    *checkpoint.Store
	Versions *version.Store
	Opts     ExecutorOptions

	mu           sync.Mutex
	sessionLocks map[string]*sync.Mutex
}

type PreviewMeta struct {
	ParamVersion      uint64  `json:"param_version"`
	ParamsHash        string  `json:"params_hash"`
	AlgorithmVersion  string  `json:"algorithm_version"`
	DownsampleFactor  int     `json:"downsample_factor"`
	IterationBudget   int     `json:"iteration_budget"`
	InitialIterations []int   `json:"initial_iterations"`
	UsedIterations    []int   `json:"used_iterations"`
	FallbackFull      bool    `json:"fallback_full"`
	Basis             string  `json:"basis"`
	LatencyMS         float64 `json:"latency_ms"`
	DeadlineMS        int     `json:"deadline_ms"`
	WithinDeadline    bool    `json:"within_deadline"`
}

type PreviewResult struct {
	Labels    []byte      `json:"-"`
	Stats     model.Stats `json:"stats"`
	MaskHash  string      `json:"mask_hash"`
	StatsHash string      `json:"stats_hash"`
	Meta      PreviewMeta `json:"meta"`
}

type RunRequest struct {
	SessionID         string
	Mode              checkpoint.OpenMode
	SnapshotVersion   uint64
	PauseAfterCommits int
}

type RunResult struct {
	SessionID        string                    `json:"session_id"`
	Mode             checkpoint.OpenMode       `json:"mode"`
	Status           string                    `json:"status"`
	Width            int                       `json:"width"`
	Height           int                       `json:"height"`
	ParamVersion     uint64                    `json:"param_version"`
	ParamsHash       string                    `json:"params_hash"`
	AlgorithmVersion string                    `json:"algorithm_version"`
	Labels           []byte                    `json:"-"`
	Stats            model.Stats               `json:"stats"`
	MaskHash         string                    `json:"mask_hash"`
	StatsHash        string                    `json:"stats_hash"`
	CommitSequence   []string                  `json:"commit_sequence"`
	Commits          []checkpoint.CommitRecord `json:"commits"`
	CommittedNew     int                       `json:"committed_new"`
	Invalidated      bool                      `json:"invalidated"`
	InvalidateReason string                    `json:"invalidate_reason,omitempty"`
	Paused           bool                      `json:"paused"`
}

type ConsistencyReport struct {
	RegionCount        int          `json:"region_count"`
	RegionCountDiff    int          `json:"region_count_diff"`
	MaxCentroidDeltaPx float64      `json:"max_centroid_delta_px"`
	MaxAreaRelError    float64      `json:"max_area_rel_error"`
	FirstRegion        int          `json:"first_region,omitempty"`
	FirstDiffPixel     *model.Point `json:"first_diff_pixel,omitempty"`
	WithinTolerance    bool         `json:"within_tolerance"`
}

func (e *Engine) init() {
	if e.Opts.MaxParallel == 0 {
		e.Opts = DefaultExecutorOptions()
	}
	e.mu.Lock()
	if e.sessionLocks == nil {
		e.sessionLocks = make(map[string]*sync.Mutex)
	}
	e.mu.Unlock()
}

func New(store *checkpoint.Store, versions *version.Store, opts ExecutorOptions) (*Engine, error) {
	if store == nil || versions == nil {
		return nil, paramError("store", "checkpoint store and version store are required")
	}
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	return &Engine{Store: store, Versions: versions, Opts: opts, sessionLocks: make(map[string]*sync.Mutex)}, nil
}

func (e *Engine) lockSession(sessionID string) (func(), error) {
	e.init()
	e.mu.Lock()
	lock := e.sessionLocks[sessionID]
	if lock == nil {
		lock = &sync.Mutex{}
		e.sessionLocks[sessionID] = lock
	}
	e.mu.Unlock()
	if !lock.TryLock() {
		return nil, paramError("session_id", "checkpoint session is already running")
	}
	return lock.Unlock, nil
}

func (e *Engine) sampleToSegmentOptions(smp *sample.Sample, params model.Params, budget int) segmentOptions {
	areas := make(map[int]int, len(smp.Manifest.Regions))
	rects := make(map[int]model.Rect, len(smp.Manifest.Regions))
	for _, r := range smp.Manifest.Regions {
		areas[r.ID] = r.Area
		rects[r.ID] = r.BBox
	}
	return segmentOptions{
		params: params, truthLabels: smp.TruthLabels, truthAreas: areas, truthRects: rects,
		budget: budget, maxAreaRatio: e.Opts.MaxAreaRatio, outsideTruthRatio: e.Opts.OutsideTruthRatio,
	}
}
