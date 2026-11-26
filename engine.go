package loudness

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// RunMode selects the reconstruction path.
type RunMode string

const (
	RunFull        RunMode = "full"
	RunIncremental RunMode = "incremental"
	RunResume      RunMode = "resume"
)

type EngineOptions struct {
	CheckpointDir string          `json:"checkpointDir"`
	Scheduler     SchedulerConfig `json:"scheduler"`
	Rounding      Rounding        `json:"rounding"`
}

// BuildResult is deterministic across full, incremental, and resumed paths.
type BuildResult struct {
	Mode               RunMode             `json:"mode"`
	Project            *Project            `json:"project"`
	Clips              []ClipResult        `json:"clips"`
	Gains              map[int]int64       `json:"gains"`
	Invalidations      []Invalidation      `json:"invalidations"`
	CommitSequence     []int               `json:"commitSequence"`
	ComputedClips      []int               `json:"computedClips"`
	MeasurementReused  []int               `json:"measurementReused"`
	CalibrationReused  []int               `json:"calibrationReused"`
	WaitEvents         []WaitEvent         `json:"waitEvents"`
	MaxObservedWorkers int                 `json:"maxObservedWorkers"`
	State              []StateSnapshot     `json:"clipState"`
	DependencyGraph    DependencySnapshot  `json:"dependencyGraph"`
	Checkpoints        []CheckpointSummary `json:"checkpointSummaries"`
}

type Engine struct {
	project       *Project
	graph         *DependencyGraph
	store         *CheckpointStore
	scheduler     SchedulerConfig
	rounding      Rounding
	invalidations []Invalidation
	mu            sync.Mutex
}

func NewEngine(project *Project, options EngineOptions) (*Engine, error) {
	if err := validateProjectBasics(project); err != nil {
		return nil, err
	}
	config := options.Scheduler
	defaults := DefaultSchedulerConfig()
	if config.MaxWorkers == 0 {
		config.MaxWorkers = defaults.MaxWorkers
	}
	if config.QueueBudgetSamples == 0 {
		config.QueueBudgetSamples = defaults.QueueBudgetSamples
	}
	if config.BufferBudgetSamples == 0 {
		config.BufferBudgetSamples = defaults.BufferBudgetSamples
	}
	if config.MaxSamplesPerWorker == 0 {
		config.MaxSamplesPerWorker = defaults.MaxSamplesPerWorker
	}
	store, err := NewCheckpointStore(options.CheckpointDir)
	if err != nil {
		return nil, err
	}
	rounding := options.Rounding
	if rounding == "" {
		rounding = RoundHalfUp
	}
	if !rounding.Valid() {
		return nil, &DomainError{Code: EPara, Path: "$.rounding", Field: "rounding", Detail: "unknown rounding mode"}
	}
	graph, err := BuildDependencyGraph(project)
	if err != nil {
		return nil, err
	}
	return &Engine{
		project: CloneProject(project), graph: graph, store: store,
		scheduler: config, rounding: rounding,
	}, nil
}

func (e *Engine) Project() *Project { return CloneProject(e.project) }

func (e *Engine) Invalidations() []Invalidation {
	return append([]Invalidation(nil), e.invalidations...)
}

// ApplyChanges validates and applies designer edits, rebuilding the graph after
// each change and returning the exact merged invalidation list.
func (e *Engine) ApplyChanges(changes []Change) ([]Invalidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	nextProject := CloneProject(e.project)
	currentGraph := e.graph
	all := make([]Invalidation, 0)
	for _, change := range changes {
		if changeIsNoop(nextProject, change) {
			continue
		}
		invalidations, err := currentGraph.MarkChange(change)
		if err != nil {
			e.graph, _ = BuildDependencyGraph(e.project)
			return nil, err
		}
		if err := ApplyChange(nextProject, change); err != nil {
			e.graph, _ = BuildDependencyGraph(e.project)
			return nil, err
		}
		nextGraph, err := BuildDependencyGraph(nextProject)
		if err != nil {
			e.graph, _ = BuildDependencyGraph(e.project)
			return nil, err
		}
		currentGraph = nextGraph
		all = append(all, invalidations...)
	}
	e.project = nextProject
	e.graph = currentGraph
	e.invalidations = mergeInvalidations(all)
	return append([]Invalidation(nil), e.invalidations...), nil
}

func changeIsNoop(project *Project, change Change) bool {
	switch change.Kind {
	case ChangeGain:
		clip, ok := project.ClipByID(change.ClipID)
		return ok && clip.GainQ12 == change.GainQ12
	case ChangeTarget:
		return project.TargetLoudnessDBQ8 == change.TargetLoudnessDBQ8
	case ChangeFrameRate:
		return project.FrameLength == change.FrameLength && project.FrameShift == change.FrameShift
	default:
		return false
	}
}

// Run executes one of the three paths. ModeFull ignores old checkpoints;
// incremental and resume modes reuse only cache-key-valid persisted work.
func (e *Engine) Run(ctx context.Context, mode RunMode) (*BuildResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch mode {
	case RunFull, RunIncremental, RunResume:
	default:
		return nil, &DomainError{Code: EPara, Path: "$.mode", Field: "mode", Detail: "unknown run mode"}
	}
	checkpoints := make(map[int]ClipCheckpoint)
	if mode != RunFull {
		all, err := e.store.ReadAll()
		if err != nil {
			return nil, err
		}
		for _, checkpoint := range all {
			checkpoints[checkpoint.ClipID] = checkpoint
		}
	}

	measurements := make(map[int]ClipMeasurement, len(e.project.Clips))
	results := make(map[int]ClipResult, len(e.project.Clips))
	needsMeasurement := make([]Clip, 0)
	needsCalibration := make([]Clip, 0)
	measurementReused := make([]int, 0)
	calibrationReused := make([]int, 0)
	computed := make([]int, 0)

	for _, clip := range e.project.Clips {
		checkpoint, exists := checkpoints[clip.ID]
		if exists {
			measurementValid := e.checkpointMeasurementValid(checkpoint, clip.ID)
			if measurementValid {
				measurement := measurementFromCheckpoint(checkpoint)
				measurements[clip.ID] = measurement
				measurementReused = append(measurementReused, clip.ID)
				if e.checkpointCalibrationValid(checkpoint, clip.ID) {
					result := ResultFromCheckpoint(checkpoint)
					e.attachCacheKeys(&result)
					results[clip.ID] = result
					calibrationReused = append(calibrationReused, clip.ID)
					continue
				}
				needsCalibration = append(needsCalibration, clip)
				continue
			}
		}
		needsMeasurement = append(needsMeasurement, clip)
	}

	measurementTasks := make([]ScheduledTask, 0, len(needsMeasurement))
	for _, clip := range needsMeasurement {
		clipCopy := clip
		measurementTasks = append(measurementTasks, ScheduledTask{
			ID: clip.ID, Label: fmt.Sprintf("measure-clip-%d", clip.ID),
			SampleCost: clip.DurationSamples(),
			Run: func(ctx context.Context) (any, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return MeasureClip(e.project, clipCopy)
			},
		})
	}
	measurementReport, err := RunScheduled(ctx, e.scheduler, measurementTasks)
	if err != nil {
		return nil, err
	}
	for id, value := range measurementReport.Results {
		measurement, ok := value.(ClipMeasurement)
		if !ok {
			return nil, &DomainError{Code: ESchema, Path: "$.scheduler", Detail: fmt.Sprintf("measurement task %d returned unexpected type", id)}
		}
		measurements[id] = measurement
	}
	for _, clip := range needsMeasurement {
		if _, ok := measurements[clip.ID]; !ok {
			return nil, &DomainError{Code: ESchema, Path: "$.scheduler", Detail: fmt.Sprintf("measurement for clip %d was not produced", clip.ID)}
		}
		needsCalibration = append(needsCalibration, clip)
		computed = append(computed, clip.ID)
	}
	sort.Ints(computed)

	calibrationTasks := make([]ScheduledTask, 0, len(needsCalibration))
	calibrationMax := make(map[int]int64, len(needsCalibration))
	for _, clip := range needsCalibration {
		clipCopy := clip
		measurement, ok := measurements[clip.ID]
		if !ok {
			return nil, &DomainError{Code: ESchema, Path: "$.clips", Detail: fmt.Sprintf("missing measurement for clip %d", clip.ID)}
		}
		calibrationTasks = append(calibrationTasks, ScheduledTask{
			ID: clip.ID, Label: fmt.Sprintf("calibrate-clip-%d", clip.ID),
			SampleCost: clip.DurationSamples(),
			Run: func(ctx context.Context) (any, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				result, maxLoudness, err := calibrateClip(e.project, clipCopy, measurement)
				if err != nil {
					return nil, err
				}
				return calibrationTaskResult{Result: result, MaxLoudness: maxLoudness}, nil
			},
		})
	}
	calibrationReport, err := RunScheduled(ctx, e.scheduler, calibrationTasks)
	if err != nil {
		return nil, err
	}
	for id, value := range calibrationReport.Results {
		calibration, ok := value.(calibrationTaskResult)
		if !ok {
			return nil, &DomainError{Code: ESchema, Path: "$.scheduler", Detail: fmt.Sprintf("calibration task %d returned unexpected type", id)}
		}
		result := calibration.Result
		e.attachCacheKeys(&result)
		results[id] = result
		calibrationMax[id] = calibration.MaxLoudness
	}
	for _, clip := range needsCalibration {
		if _, ok := results[clip.ID]; !ok {
			return nil, &DomainError{Code: ESchema, Path: "$.scheduler", Detail: fmt.Sprintf("calibration for clip %d was not produced", clip.ID)}
		}
	}

	report := &CalibrationReport{
		TargetLoudnessDBQ8: e.project.TargetLoudnessDBQ8,
		ReachableUpperDBQ8: int64(^uint64(0) >> 1),
		Clips:              results,
	}
	limited := make([]int, 0)
	for _, clip := range e.project.Clips {
		measurement, ok := measurements[clip.ID]
		if !ok {
			return nil, &DomainError{Code: ESchema, Path: "$.clips", Detail: fmt.Sprintf("missing measurement for clip %d", clip.ID)}
		}
		maxLoudness, ok := calibrationMax[clip.ID]
		if !ok {
			maxLoudness, err = maxClipLoudness(e.project, measurement)
			if err != nil {
				return nil, err
			}
		}
		if maxLoudness < report.ReachableUpperDBQ8 {
			report.ReachableUpperDBQ8 = maxLoudness
		}
		result, exists := results[clip.ID]
		if !exists {
			return nil, &DomainError{Code: ESchema, Path: "$.clips", Detail: fmt.Sprintf("missing result for clip %d", clip.ID)}
		}
		if result.GainLimited || maxLoudness < e.project.TargetLoudnessDBQ8-e.project.LoudnessToleranceQ8 {
			limited = append(limited, clip.ID)
		}
	}
	sort.Ints(limited)
	if report.ReachableUpperDBQ8 < e.project.TargetLoudnessDBQ8-e.project.LoudnessToleranceQ8 {
		return nil, &DomainError{
			Code: ELoud, Path: "$.targetLoudnessDBQ8",
			Target: e.project.TargetLoudnessDBQ8, Reachable: report.ReachableUpperDBQ8,
			LimitedClips: limited,
			Detail:       fmt.Sprintf("target q8=%d exceeds reachable upper q8=%d", e.project.TargetLoudnessDBQ8, report.ReachableUpperDBQ8),
		}
	}

	state, err := NewStateMachine(e.project.ClipIDs())
	if err != nil {
		return nil, err
	}
	commitSequence := e.project.ClipIDs()
	for commitIndex, id := range commitSequence {
		result, ok := results[id]
		if !ok {
			return nil, &DomainError{Code: ESchema, Path: "$.clips", Detail: fmt.Sprintf("missing result for clip %d", id)}
		}
		if err := state.Transition(id, StateMeasuring); err != nil {
			return nil, err
		}
		if err := state.Transition(id, StateCalibrating); err != nil {
			return nil, err
		}
		checkpoint := checkpointFromResult(e.project, result, commitIndex)
		if err := e.store.Write(ctx, checkpoint); err != nil {
			_ = state.Transition(id, StateFailed)
			return nil, err
		}
		if err := state.Commit(id, result); err != nil {
			return nil, err
		}
	}
	summaries, err := e.store.Summaries()
	if err != nil {
		return nil, err
	}
	clips := make([]ClipResult, 0, len(results))
	gains := make(map[int]int64, len(results))
	for _, id := range commitSequence {
		result := cloneClipResult(results[id])
		clips = append(clips, result)
		gains[id] = result.GainAdjustmentQ12
	}
	sort.Ints(measurementReused)
	sort.Ints(calibrationReused)
	waitEvents := append([]WaitEvent(nil), measurementReport.WaitEvents...)
	waitEvents = append(waitEvents, calibrationReport.WaitEvents...)
	sort.SliceStable(waitEvents, func(i, j int) bool {
		if waitEvents[i].TaskID != waitEvents[j].TaskID {
			return waitEvents[i].TaskID < waitEvents[j].TaskID
		}
		return waitEvents[i].Reason < waitEvents[j].Reason
	})
	return &BuildResult{
		Mode: mode, Project: CloneProject(e.project), Clips: clips, Gains: gains,
		Invalidations:  append([]Invalidation(nil), e.invalidations...),
		CommitSequence: commitSequence, ComputedClips: computed,
		MeasurementReused: measurementReused, CalibrationReused: calibrationReused,
		WaitEvents: waitEvents, MaxObservedWorkers: maxInt(measurementReport.MaxObservedWorkers, calibrationReport.MaxObservedWorkers),
		State: state.Snapshots(), DependencyGraph: e.graph.Snapshot(), Checkpoints: summaries,
	}, nil
}

type calibrationTaskResult struct {
	Result      ClipResult
	MaxLoudness int64
}

func (e *Engine) checkpointMeasurementValid(checkpoint ClipCheckpoint, clipID int) bool {
	if checkpoint.ProjectID != e.project.ID || checkpoint.Seed != e.project.Seed {
		return false
	}
	if checkpoint.SourceVersion != e.project.Provenance.OriginalVersion {
		return false
	}
	if checkpoint.CurrentVersion != e.project.FormatVersion {
		return false
	}
	if checkpoint.QuantVersion != e.project.Quantization.Version || checkpoint.AlgorithmVersion != AlgorithmVersion {
		return false
	}
	if checkpoint.FrameLength != e.project.FrameLength || checkpoint.FrameShift != e.project.FrameShift {
		return false
	}
	if checkpoint.ContentHash != e.graph.ContentHash(clipID) {
		return false
	}
	return checkpoint.MeasurementCacheKey == e.graph.MeasurementCacheKey(clipID)
}

func (e *Engine) checkpointCalibrationValid(checkpoint ClipCheckpoint, clipID int) bool {
	if !e.checkpointMeasurementValid(checkpoint, clipID) {
		return false
	}
	if checkpoint.TargetLoudnessDBQ8 != e.project.TargetLoudnessDBQ8 ||
		checkpoint.SessionGainQ12 != e.project.SessionGainQ12 ||
		checkpoint.MaxGainQ12 != e.project.MaxGainQ12 ||
		checkpoint.LoudnessToleranceQ8 != e.project.LoudnessToleranceQ8 {
		return false
	}
	return checkpoint.CalibrationCacheKey == e.graph.CalibrationCacheKey(clipID)
}

func (e *Engine) attachCacheKeys(result *ClipResult) {
	frameKeys := e.graph.FrameCacheKeys(result.ClipID)
	for i := range result.Frames {
		if i < len(frameKeys) {
			result.Frames[i].CacheKey = frameKeys[i]
		}
	}
	result.MeasurementCacheKey = e.graph.MeasurementCacheKey(result.ClipID)
	result.CalibrationCacheKey = e.graph.CalibrationCacheKey(result.ClipID)
}

func maxClipLoudness(project *Project, measurement ClipMeasurement) (int64, error) {
	currentRms, _, err := MulDivAt(fmt.Sprintf("$.clips[%d].maxLoudness.session", indexOfClip(project, measurement.ClipID)), measurement.SegmentRmsQ16, project.SessionGainQ12, InternalGainScale, RoundHalfUp)
	if err != nil {
		return 0, err
	}
	maxRms, _, err := MulDivAt(fmt.Sprintf("$.clips[%d].maxLoudness.gain", indexOfClip(project, measurement.ClipID)), currentRms, project.MaxGainQ12, InternalGainScale, RoundHalfUp)
	if err != nil {
		return 0, err
	}
	db, _, err := LinearToDBQ16(maxRms, fmt.Sprintf("$.clips[%d].maxLoudness.db", indexOfClip(project, measurement.ClipID)))
	return db, err
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
