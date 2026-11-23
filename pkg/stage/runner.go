package stage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type RunMode string

const (
	RunRealtime RunMode = "realtime"
	RunResume   RunMode = "resume"
	RunFull     RunMode = "full"
)

type RunOptions struct {
	Mode           RunMode
	CheckpointPath string
	StopAfter      int
	FaultInjection bool
	FaultWindow    int
	FaultMessage   string
	SkipTruthCheck bool
}

type RunResult struct {
	ProjectID             string         `json:"project_id"`
	Mode                  RunMode        `json:"run_mode"`
	Decision              ModeDecision   `json:"decision"`
	Windows               []WindowResult `json:"windows"`
	CommitOrder           []int          `json:"commit_order"`
	BudgetLog             []BudgetRecord `json:"budget_log"`
	Statuses              []WindowStatus `json:"statuses"`
	Progress              Progress       `json:"progress"`
	Partial               bool           `json:"partial"`
	CheckpointInvalidated bool           `json:"checkpoint_invalidated"`
	InvalidationReason    string         `json:"invalidation_reason,omitempty"`
	StartedAt             time.Time      `json:"started_at"`
	FinishedAt            time.Time      `json:"finished_at"`
}

type Runner struct {
	project   *Project
	params    Params
	evaluator *Evaluator
	decision  ModeDecision
	specs     []WindowSpec
}

func NewRunner(project *Project, params Params) (*Runner, error) {
	params = params.WithDefaults(project)
	evaluator, err := NewEvaluator(project, params)
	if err != nil {
		return nil, err
	}
	return &Runner{
		project:   project,
		params:    params,
		evaluator: evaluator,
		decision:  SelectMode(project, params),
		specs:     BuildWindowSpecs(project, params),
	}, nil
}

func (r *Runner) Decision() ModeDecision { return r.decision }

func (r *Runner) Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	started := time.Now().UTC()
	result := RunResult{
		ProjectID: r.project.ID,
		Mode:      opts.Mode,
		Decision:  r.decision,
		StartedAt: started,
	}
	if opts.Mode == "" {
		opts.Mode = RunRealtime
	}
	if opts.Mode != RunRealtime && opts.Mode != RunResume && opts.Mode != RunFull {
		return result, newError(CodeParameter, "unsupported run mode", "run", "mode")
	}
	if opts.CheckpointPath == "" {
		return result, newError(CodeParameter, "checkpoint path is required", "run", "checkpoint_path")
	}
	if opts.StopAfter < 0 {
		return result, newError(CodeParameter, "stop_after must be non-negative", "run", "stop_after")
	}
	store, err := OpenCheckpoint(opts.CheckpointPath)
	if err != nil {
		return result, err
	}
	defer store.Close()
	if opts.Mode == RunFull {
		if err := store.Reset(r.project.ID); err != nil {
			return result, err
		}
	}
	contentHash := r.evaluator.ContentHash()
	paramVersion := r.evaluator.ParamVersion()
	loaded, err := store.Load(r.project.ID, contentHash, paramVersion, AlgorithmVersion, len(r.specs))
	if err != nil {
		return result, err
	}
	result.CheckpointInvalidated = loaded.Invalidated
	result.InvalidationReason = loaded.Reason
	if opts.Mode == RunFull && len(loaded.Windows) > 0 {
		return result, newError(CodeState, "full recompute unexpectedly loaded checkpoint windows", "checkpoint", "mode")
	}
	machine := NewWindowMachine(r.specs)
	for _, window := range loaded.Windows {
		if err := machine.InstallCommitted(window); err != nil {
			return result, err
		}
		result.CommitOrder = append(result.CommitOrder, window.Index)
	}
	startIndex := len(loaded.Windows)
	if startIndex == len(r.specs) {
		result.Windows, err = committedWindows(machine)
		if err != nil {
			return result, err
		}
		result.Progress = machine.Progress()
		result.Statuses = machine.Statuses()
		result.FinishedAt = time.Now().UTC()
		return result, nil
	}
	if !opts.SkipTruthCheck && len(r.project.Truth.Samples) > 0 {
		if err := VerifyTruth(r.project, r.evaluator); err != nil {
			return result, err
		}
		if r.decision.Mode == ModeMerged {
			if err := CheckCrossModeSamples(r.project, r.params, r.evaluator); err != nil {
				return result, err
			}
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	budget, runErr := r.schedule(runCtx, cancel, store, machine, startIndex, opts, &result)
	if runErr == nil {
		var err error
		result.Windows, err = committedWindows(machine)
		if err != nil {
			return result, err
		}
		result.Progress = machine.Progress()
		result.Partial = result.Progress.Committed < result.Progress.Total
	} else if errors.Is(runErr, context.Canceled) {
		result.Windows, _ = committedWindows(machine)
		result.Progress = machine.Progress()
		result.Partial = result.Progress.Committed < result.Progress.Total
	} else {
		result.Windows, _ = committedWindows(machine)
		result.Progress = machine.Progress()
		result.Partial = true
	}
	result.BudgetLog = budget.Records()
	result.Statuses = machine.Statuses()
	result.FinishedAt = time.Now().UTC()
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return result, runErr
	}
	return result, nil
}

func (r *Runner) schedule(ctx context.Context, cancel context.CancelFunc, store *CheckpointStore, machine *WindowMachine, startIndex int, opts RunOptions, result *RunResult) (*memoryBudget, error) {
	type task struct{ index int }
	type taskResult struct {
		index    int
		window   WindowResult
		err      error
		acquired int64
	}
	total := len(r.specs)
	budget := newMemoryBudget(r.params.MemoryBudgetBytes)
	resultCh := make(chan taskResult, total)
	jobs := make(chan task, r.params.MaxWorkers*2)
	inflight := make(chan struct{}, r.params.MaxWorkers)
	var startMu sync.Mutex
	startCond := sync.NewCond(&startMu)
	nextStart := startIndex
	go func() {
		<-ctx.Done()
		startMu.Lock()
		startCond.Broadcast()
		startMu.Unlock()
	}()
	waitStart := func(index int) bool {
		startMu.Lock()
		defer startMu.Unlock()
		for index != nextStart {
			if ctx.Err() != nil {
				return false
			}
			startCond.Wait()
		}
		nextStart++
		startCond.Broadcast()
		return true
	}
	var wg sync.WaitGroup
	for worker := 0; worker < r.params.MaxWorkers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				tr := taskResult{index: job.index}
				select {
				case <-ctx.Done():
					tr.err = ctx.Err()
					resultCh <- tr
					continue
				default:
				}
				if !waitStart(job.index) {
					tr.err = ctx.Err()
					resultCh <- tr
					continue
				}
				need := estimateWindowBytes(r.project, r.specs[job.index], r.decision)
				if need > r.params.PerWorkerBufferBytes {
					reason := "window exceeds per-worker buffer limit"
					budget.Record(BudgetRecord{
						Window: job.index, Event: "rejected", Required: need,
						Available: budget.available(), Max: r.params.PerWorkerBufferBytes, Reason: reason,
					})
					tr.err = withDetail(
						newError(CodeState, reason, fmt.Sprintf("window:%d", job.index), "per_worker_buffer_bytes"),
						ErrorDetail{Expected: fmt.Sprintf("<=%d", r.params.PerWorkerBufferBytes), Actual: fmt.Sprint(need), Window: job.index},
					)
					if beginErr := machine.Begin(job.index); beginErr == nil {
						_ = machine.Fail(job.index, tr.err)
					}
					resultCh <- tr
					continue
				}
				acquired, err := budget.Acquire(ctx, job.index, need)
				if err != nil {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						if beginErr := machine.Begin(job.index); beginErr == nil {
							_ = machine.Fail(job.index, err)
						}
					}
					tr.err = err
					resultCh <- tr
					continue
				}
				tr.acquired = acquired
				if err := machine.Begin(job.index); err != nil {
					tr.err = err
					resultCh <- tr
					continue
				}
				if opts.FaultInjection && job.index == opts.FaultWindow {
					message := opts.FaultMessage
					if message == "" {
						message = "injected window failure"
					}
					fault := newError(CodeState, message, fmt.Sprintf("window:%d", job.index), "fault_injection")
					_ = machine.Fail(job.index, fault)
					tr.err = fault
					resultCh <- tr
					continue
				}
				window, err := ComputeWindow(r.project, r.evaluator, r.specs[job.index], r.decision)
				if err != nil {
					_ = machine.Fail(job.index, err)
					tr.err = err
					resultCh <- tr
					continue
				}
				tr.window = window
				resultCh <- tr
			}
		}()
	}
	go func() {
		for index := startIndex; index < total; index++ {
			select {
			case inflight <- struct{}{}:
			case <-ctx.Done():
				close(jobs)
				return
			}
			select {
			case jobs <- task{index: index}:
			case <-ctx.Done():
				close(jobs)
				return
			}
		}
		close(jobs)
	}()
	committedNew := 0
	expected := startIndex
	pending := make(map[int]taskResult)
	for expected < total {
		tr, ready := pending[expected]
		if ready {
			delete(pending, expected)
		} else {
			select {
			case tr = <-resultCh:
			case <-ctx.Done():
				wg.Wait()
				return budget, context.Canceled
			}
		}
		if tr.index != expected {
			pending[tr.index] = tr
			continue
		}
		if tr.err != nil {
			budget.Release(tr.acquired)
			<-inflight
			cancel()
			wg.Wait()
			if errors.Is(tr.err, context.Canceled) {
				return budget, context.Canceled
			}
			return budget, tr.err
		}
		if err := machine.Commit(tr.window); err != nil {
			budget.Release(tr.acquired)
			<-inflight
			cancel()
			wg.Wait()
			return budget, err
		}
		if err := store.SaveWindow(r.project.ID, tr.window, total); err != nil {
			budget.Release(tr.acquired)
			<-inflight
			cancel()
			wg.Wait()
			return budget, err
		}
		result.CommitOrder = append(result.CommitOrder, tr.index)
		budget.Release(tr.acquired)
		<-inflight
		committedNew++
		expected++
		if opts.StopAfter > 0 && committedNew >= opts.StopAfter {
			cancel()
			wg.Wait()
			return budget, nil
		}
	}
	wg.Wait()
	return budget, nil
}

func committedWindows(machine *WindowMachine) ([]WindowResult, error) {
	total := machine.Total()
	windows := make([]WindowResult, 0, total)
	for index := 0; index < total; index++ {
		state, err := machine.State(index)
		if err != nil {
			return nil, err
		}
		if state != WindowCommitted {
			break
		}
		window, err := machine.Result(index)
		if err != nil {
			return nil, err
		}
		windows = append(windows, *window)
	}
	return windows, nil
}

func estimateWindowBytes(project *Project, spec WindowSpec, decision ModeDecision) int64 {
	frames := int64(spec.FrameEnd - spec.FrameStart)
	if frames <= 0 {
		return 1
	}
	perLight := int64(192)
	bytes := frames * int64(len(project.Lights)) * perLight
	bytes += int64(spec.CueOffset-spec.CueStart) * 256
	if decision.Mode == ModeMerged {
		bytes += frames * 96
	}
	return bytes
}

func (r RunResult) Snapshot() PathSnapshot {
	snapshot := PathSnapshot{
		CommitOrder: append([]int(nil), r.CommitOrder...),
	}
	for _, window := range r.Windows {
		firstFrame := 0
		if len(window.Frames) > 0 {
			firstFrame = window.Frames[0].Frame
		}
		snapshot.WindowFirstFrames = append(snapshot.WindowFirstFrames, firstFrame)
		snapshot.CueOffsets = append(snapshot.CueOffsets, window.CueOffset)
		snapshot.Frames = append(snapshot.Frames, window.Frames...)
	}
	return snapshot
}
