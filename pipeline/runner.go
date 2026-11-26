package pipeline

import (
	"context"
	"encoding/json"
	"sync"

	"iconflow/checkpoint"
	"iconflow/errs"
	"iconflow/graph"
	"iconflow/model"
	"iconflow/segment"
	"iconflow/state"
)

type Runner struct {
	Project *model.Project
	Options Options
	Store   checkpoint.Store

	blocks     map[string]*state.BlockMachine
	windows    map[string]*state.WindowMachine
	history    map[string][]checkpoint.StageCommit
	attempts   map[string]int
	attemptsMu sync.Mutex
	scheduler  *Scheduler
}

func NewRunner(ctx context.Context, project *model.Project, options Options) (*Runner, *errs.Error) {
	if project == nil {
		return nil, errs.New(errs.EInternal, "$.project", "project is nil")
	}
	store := options.Store
	if store == nil {
		store = checkpoint.NewMemoryStore()
	}
	budget := options.Budget
	if budget.Workers == 0 {
		budget = DefaultBudget()
	}
	runner := &Runner{
		Project: project, Options: options, Store: store,
		blocks:   map[string]*state.BlockMachine{},
		windows:  map[string]*state.WindowMachine{},
		history:  map[string][]checkpoint.StageCommit{},
		attempts: map[string]int{},
	}
	for _, coord := range project.SortedBlockCoords() {
		runner.blocks[coord.Key()] = state.NewBlockMachine(coord.Key())
	}
	for _, panel := range project.Panels {
		runner.windows[panel.ID] = state.NewWindowMachine(panel.ID)
	}
	history, err := store.StageHistory(ctx, project.ProjectID)
	if err != nil {
		return nil, errs.New(errs.ECheck, "$.checkpoint", "load stage history: %v", err)
	}
	for _, commit := range history {
		runner.history[commit.BlockKey] = append(runner.history[commit.BlockKey], commit)
	}
	windowHistory, werr := store.WindowHistory(ctx, project.ProjectID)
	if werr != nil {
		return nil, errs.New(errs.ECheck, "$.checkpoint", "load window history: %v", werr)
	}
	for _, commit := range windowHistory {
		machine := runner.windows[commit.WindowID]
		if machine == nil {
			machine = state.NewWindowMachine(commit.WindowID)
			runner.windows[commit.WindowID] = machine
		}
		if commit.Status == string(state.WindowCommitted) && machine.Status == state.WindowPending {
			_ = machine.Transition(state.WindowSegmenting)
			_ = machine.Transition(state.WindowCommitted)
		}
		if commit.Status == string(state.WindowFailed) && machine.Status == state.WindowPending {
			_ = machine.Transition(state.WindowSegmenting)
			_ = machine.Transition(state.WindowFailed)
		}
	}
	return runner, nil
}

func (r *Runner) Close() error {
	if r.scheduler != nil {
		r.scheduler.Close()
	}
	return r.Store.Close()
}

func (r *Runner) findCommit(blockKey string, stage state.Stage, cacheKey string) (checkpoint.StageCommit, bool) {
	for _, commit := range r.history[blockKey] {
		if commit.Stage == string(stage) && commit.CacheKey == cacheKey && commit.Status == string(state.StatusCommitted) {
			return commit, true
		}
	}
	return checkpoint.StageCommit{}, false
}

func (r *Runner) reusableResult(coord model.BlockCoord) (*BlockResult, bool) {
	inputHash := graph.BlockInputHash(r.Project.ImageByID(coord.ImageID), coord)
	keys := KeysFor(r.Project, coord, inputHash)
	commit, ok := r.findCommit(coord.Key(), state.StageCleaned, keys.Stats)
	if !ok || len(commit.ArtifactJSON) == 0 {
		return nil, false
	}
	var artifact Artifact
	if err := json.Unmarshal(commit.ArtifactJSON, &artifact); err != nil || artifact.Stats == nil {
		return nil, false
	}
	result := &BlockResult{
		Coord: coord, InputHash: inputHash, Keys: keys,
		Gradient: artifact.Gradient, Threshold: artifact.Threshold,
		Markers: artifact.Markers, Stats: artifact.Stats,
		Reused: true, StageSeqs: map[string]int64{},
	}
	if artifact.Stats != nil {
		result.ValidationError = segment.ValidateBlock(artifact.Stats, r.Project.Truth[coord.Key()], r.Project.Rules.Tolerances)
	}
	for _, stage := range state.Stages() {
		cacheKey := r.cacheKeyForStage(stage, keys, inputHash)
		if stageCommit, found := r.findCommit(coord.Key(), stage, cacheKey); found {
			result.ReusedStages = append(result.ReusedStages, string(stage))
			result.StageSeqs[string(stage)] = stageCommit.Seq
			_ = r.blocks[coord.Key()].Commit(stage)
		}
	}
	return result, true
}

func (r *Runner) cacheKeyForStage(stage state.Stage, keys StageKeys, inputHash string) string {
	switch stage {
	case state.StageLoaded:
		return loadedCacheKey(inputHash)
	case state.StageGradient:
		return keys.Gradient
	case state.StageThreshold:
		return keys.Threshold
	case state.StageMarked:
		return keys.Markers
	case state.StageCleaned:
		return keys.Stats
	default:
		return ""
	}
}

type future struct {
	result *BlockResult
	handle *JobHandle
}

func (r *Runner) Run(ctx context.Context) (*RunReport, *errs.Error) {
	coords := r.Project.SortedBlockCoords()
	budget := r.Options.Budget
	if budget.Workers == 0 {
		budget = DefaultBudget()
	}
	r.scheduler = NewScheduler(budget)

	futures := make([]future, len(coords))
	for i, coord := range coords {
		if result, ok := r.reusableResult(coord); ok {
			futures[i] = future{result: result}
			continue
		}
		coord := coord
		pixels := coord.Width * coord.Height
		futures[i] = future{handle: r.scheduler.Submit(coord, pixels, func() (*BlockResult, *JobError) {
			key := coord.Key()
			r.attemptsMu.Lock()
			r.attempts[key]++
			attempt := r.attempts[key]
			r.attemptsMu.Unlock()
			if r.Options.FailFirstAttempt[key] && attempt == 1 {
				e := errs.New(errs.EInternal, key, "injected deterministic first-attempt failure")
				return &BlockResult{Coord: coord}, &JobError{Coord: coord, Stage: state.StageGradient, Err: e}
			}
			return ComputeBlock(r.Project, coord, budget.MaxPixelsPerThread)
		})}
	}

	report := &RunReport{ProjectID: r.Project.ProjectID, Mode: r.Options.Mode}
	committedBlocks := 0
	for i, coord := range coords {
		var result *BlockResult
		if futures[i].result != nil {
			result = futures[i].result
		} else {
			jobResult := futures[i].handle.Wait(ctx)
			if jobResult.Error != nil {
				_ = r.blocks[coord.Key()].Fail(jobResult.Error.Stage)
				if jobResult.Block != nil {
					result = jobResult.Block
				} else {
					result = &BlockResult{Coord: coord, StageSeqs: map[string]int64{}}
				}
				result.ValidationError = jobResult.Error.Err
				report.Results = append(report.Results, result)
				continue
			}
			result = jobResult.Block
		}
		if result.StageSeqs == nil {
			result.StageSeqs = map[string]int64{}
		}
		if err := r.commitResult(ctx, result); err != nil {
			report.Results = append(report.Results, result)
			continue
		}
		report.Results = append(report.Results, result)
		if r.blocks[coord.Key()].Frozen() {
			committedBlocks++
		}
		if r.Options.StopAfterBlocks > 0 && committedBlocks >= r.Options.StopAfterBlocks {
			report.Interrupted = true
			r.scheduler.Close()
			r.scheduler = nil
			sequence, _ := r.Store.StageHistory(ctx, r.Project.ProjectID)
			report.CommitSequence = toSequence(sequence)
			report.Progress = r.progress(len(coords))
			return report, ErrInterrupted
		}
	}
	queueEvents := r.scheduler.Events()
	r.scheduler.Close()
	r.scheduler = nil
	if flushErr := r.flushWindows(ctx, report); flushErr != nil {
		return report, flushErr
	}

	history, err := r.Store.StageHistory(ctx, r.Project.ProjectID)
	if err != nil {
		return report, errs.New(errs.ECheck, "$.checkpoint", "read stage history: %v", err)
	}
	report.CommitSequence = toSequence(history)
	report.QueueEvents = append(report.QueueEvents, queueEvents...)
	report.Progress = r.progress(len(coords))
	for _, result := range report.Results {
		if result.ValidationError != nil && result.ValidationError.Code != errs.EInternal {
			report.ValidationFailed++
		}
	}
	return report, nil
}

func (r *Runner) progress(totalBlocks int) state.Progress {
	return state.Progress{
		CommittedBlocks:  state.CountCommittedBlocks(r.blocks),
		TotalBlocks:      totalBlocks,
		CommittedWindows: state.CountCommittedWindows(r.windows),
		TotalWindows:     len(r.windows),
	}
}

func toSequence(commits []checkpoint.StageCommit) []CommitSequenceItem {
	out := make([]CommitSequenceItem, 0, len(commits))
	for _, commit := range commits {
		out = append(out, CommitSequenceItem{
			Seq: commit.Seq, BlockKey: commit.BlockKey, Stage: commit.Stage,
			Status: commit.Status, CacheKey: commit.CacheKey, Threshold: commit.Threshold,
			InputHash: commit.InputHash, ContentHash: commit.ContentHash,
		})
	}
	return out
}
