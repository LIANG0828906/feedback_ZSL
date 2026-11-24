package pipeline

import (
	"context"
	"sort"
	"sync"
	"time"

	"audiocheck/internal/bound"
	"audiocheck/internal/eval"
	"audiocheck/internal/generator"
	"audiocheck/internal/model"
	"audiocheck/internal/store"
)

type FailureInjector struct {
	mu           sync.Mutex
	FailAttempts map[int]int
	FailAlways   map[int]bool
	Calls        map[int]int
}

func NewFailureInjector() *FailureInjector {
	return &FailureInjector{Calls: map[int]int{}}
}

func (f *FailureInjector) BeforeEvaluate(block int) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls[block]++
	if f.FailAlways != nil && f.FailAlways[block] {
		return model.NewError(model.CodeStage, "injected_stage_failure", "injected persistent evaluate failure in block %d", block)
	}
	if f.FailAttempts != nil && f.FailAttempts[block] > 0 {
		f.FailAttempts[block]--
		return model.NewError(model.CodeStage, "injected_stage_failure", "injected evaluate failure in block %d", block)
	}
	return nil
}

type Hooks struct {
	OnWorkerStart func(blockIndex, workerID int)
}

type Config struct {
	Manifest       model.Manifest
	Sequence       *model.Sequence
	Truth          *generator.Truth
	Quant          []model.FrameQuantRecord
	Params         model.EvalParams
	Budget         model.Budget
	Store          *store.Store
	Failure        *FailureInjector
	Hooks          Hooks
	MaxAttempts    int
	PersistSamples bool
	Force          bool
	Mode           string
}

type task struct {
	block       int
	plan        *eval.BlockPlan
	submitStart time.Time
	queue       model.QueueRecord
}

type outcome struct {
	block model.BlockResult
	err   error
}

func BuildIdentity(m model.Manifest, seq *model.Sequence, truthFP string, params model.EvalParams) (model.Identity, error) {
	origHash, err := model.HashCanonical(seq.Original)
	if err != nil {
		return model.Identity{}, err
	}
	reconHash, err := model.HashCanonical(seq.Reconstructed)
	if err != nil {
		return model.Identity{}, err
	}
	content := origHash
	paramHash, err := model.HashCanonical(struct {
		Manifest model.Manifest
		Params   model.EvalParams
	}{m, MetricParamsForHash(m, params)})
	if err != nil {
		return model.Identity{}, err
	}
	return model.Identity{
		ContentHash:        content,
		OriginalHash:       origHash,
		ReconstructionHash: reconHash,
		TruthFingerprint:   truthFP,
		ParamVersion:       model.ParamSpecVersion,
		ParamHash:          paramHash,
		AlgorithmVersion:   model.AlgorithmVersion,
		SchemaVersion:      model.CheckpointSchemaVersion,
	}, nil
}

func MetricParamsForHash(m model.Manifest, p model.EvalParams) model.EvalParams {
	p = p.WithDefaults(m)
	p.SideBandSamples = 0
	return p
}

func DefaultBudget() model.Budget {
	return model.Budget{ChunkFrames: 4, MaxWorkers: 2, PerThreadSamples: 262144, MaxPendingTasks: 2}
}

func Run(ctx context.Context, cfg Config) (*model.Report, error) {
	if cfg.Sequence == nil {
		return nil, model.NewError(model.CodeSchema, "sequence", "missing generated sequence")
	}
	if err := cfg.Manifest.ValidateSchema(); err != nil {
		return nil, err
	}
	if err := cfg.Manifest.ValidateParams(); err != nil {
		return nil, err
	}
	params := cfg.Params.WithDefaults(cfg.Manifest)
	if err := params.Validate(cfg.Manifest); err != nil {
		return nil, err
	}
	budget := cfg.Budget
	if budget.ChunkFrames <= 0 {
		budget.ChunkFrames = DefaultBudget().ChunkFrames
	}
	if budget.MaxWorkers <= 0 {
		budget.MaxWorkers = DefaultBudget().MaxWorkers
	}
	if budget.PerThreadSamples <= 0 {
		budget.PerThreadSamples = DefaultBudget().PerThreadSamples
	}
	if budget.MaxPendingTasks <= 0 {
		budget.MaxPendingTasks = DefaultBudget().MaxPendingTasks
	}
	cfg.Params = params
	cfg.Budget = budget
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}

	chunkCount := (cfg.Manifest.FrameCount + budget.ChunkFrames - 1) / budget.ChunkFrames
	plans := make([]*eval.BlockPlan, chunkCount)
	for i := 0; i < chunkCount; i++ {
		plan, err := eval.PlanBlock(cfg.Manifest, params, i, budget.ChunkFrames)
		if err != nil {
			return &model.Report{Manifest: cfg.Manifest, EvalParams: params, Budget: budget}, err
		}
		if plan.Coord.BufferSamples > budget.PerThreadSamples {
			err := model.NewError(model.CodeResource, "per_thread_samples",
				"block %d needs %d buffered samples, limit is %d", i, plan.Coord.BufferSamples, budget.PerThreadSamples)
			model.WithBlock(err, plan.Coord)
			return &model.Report{Manifest: cfg.Manifest, EvalParams: params, Budget: budget}, err
		}
		plans[i] = plan
	}

	existing := map[int]model.BlockResult{}
	if cfg.Store != nil {
		blocks, err := cfg.Store.ListBlocks(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range blocks {
			existing[b.Block.Index] = b
		}
		if err := cfg.Store.PruneBlocks(ctx, chunkCount); err != nil {
			return nil, err
		}
	}

	tasks := make(chan task, budget.MaxPendingTasks)
	outcomesCh := make(chan outcome, chunkCount)
	var wg sync.WaitGroup
	for workerID := 0; workerID < budget.MaxWorkers; workerID++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for tk := range tasks {
				started := time.Now()
				tk.queue.WorkerID = id
				waitNS := started.Sub(tk.submitStart).Nanoseconds()
				if tk.queue.Reason == "pending_task_queue_full" {
					tk.queue.EnqueueWaitNS = waitNS
				} else {
					tk.queue.ScheduleWaitNS = waitNS
					if waitNS > 0 {
						tk.queue.Reason = "worker_pool_busy"
					}
				}
				tk.queue.TotalWaitNS = waitNS
				block, err := executeWithRetry(ctx, cfg, tk.plan, id, tk.queue)
				if err != nil {
					var pe *model.Error
					if asModelError(err, &pe) {
						if pe.Frame == nil {
							model.WithFrame(pe, tk.plan.Coord.FirstFrame)
						}
						model.WithBlock(pe, tk.plan.Coord)
					}
				}
				outcomesCh <- outcome{block: block, err: err}
			}
		}(workerID)
	}

	go func() {
		defer close(outcomesCh)
		for i, plan := range plans {
			if !cfg.Force {
				if reusableBlock(oldOrZero(existing, i), plan, mustParamHash(cfg)) {
					continue
				}
			}
			reason := ""
			if len(tasks) >= budget.MaxPendingTasks {
				reason = "pending_task_queue_full"
			}
			queueRecord := model.QueueRecord{
				BlockIndex: i, Reason: reason,
				QueueDepthAtSubmit: len(tasks) + 1,
				MaxBufferSamples:   budget.PerThreadSamples,
			}
			tk := task{block: i, plan: plan, submitStart: time.Now(), queue: queueRecord}
			select {
			case tasks <- tk:
			case <-ctx.Done():
				close(tasks)
				wg.Wait()
				return
			}
		}
		close(tasks)
		wg.Wait()
	}()

	outcomes := map[int]outcome{}
	for out := range outcomesCh {
		idx := out.block.Block.Index
		outcomes[idx] = out
	}

	commitOrder := []int{}
	var firstErr error
	for i := 0; i < chunkCount; i++ {
		if !cfg.Force {
			if old, ok := existing[i]; ok && reusableBlock(old, plans[i], mustParamHash(cfg)) {
				commitOrder = append(commitOrder, i)
				continue
			}
		}
		out, ok := outcomes[i]
		if !ok {
			if firstErr == nil {
				firstErr = model.NewError(model.CodeStage, "chunk_scheduling", "missing outcome for block %d", i)
			}
			break
		}
		if out.err != nil {
			if firstErr == nil {
				firstErr = out.err
			}
			break
		}
		if cfg.Store != nil {
			seq, err := cfg.Store.CommitBlock(ctx, out.block)
			if err != nil {
				if firstErr == nil {
					firstErr = model.NewError(model.CodeStage, "sqlite_commit", "block %d commit failed: %v", i, err)
				}
				break
			}
			out.block.CommitSeq = seq
		}
		commitOrder = append(commitOrder, i)
	}

	report, rerr := buildReport(ctx, cfg, existing, outcomes, commitOrder)
	if rerr != nil && firstErr == nil {
		firstErr = rerr
	}
	return report, firstErr
}

func mustParamHash(cfg Config) string {
	h, _ := model.HashCanonical(struct {
		Manifest model.Manifest
		Params   model.EvalParams
	}{cfg.Manifest, MetricParamsForHash(cfg.Manifest, cfg.Params)})
	return h
}

func oldOrZero(m map[int]model.BlockResult, i int) model.BlockResult {
	return m[i]
}

func reusableBlock(old model.BlockResult, plan *eval.BlockPlan, paramHash string) bool {
	return old.ParamVersion == model.ParamSpecVersion &&
		old.ParamHash == paramHash &&
		old.AlgorithmVersion == model.AlgorithmVersion &&
		old.Block.FirstFrame == plan.Coord.FirstFrame &&
		old.Block.LastFrame == plan.Coord.LastFrame
}

func asModelError(err error, target **model.Error) bool {
	for err != nil {
		if e, ok := err.(*model.Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func executeWithRetry(ctx context.Context, cfg Config, plan *eval.BlockPlan, workerID int, queue model.QueueRecord) (model.BlockResult, error) {
	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		result, err := executeChunk(ctx, cfg, plan, workerID, queue)
		if err == nil {
			return result, nil
		}
		lastErr = err
		injectorFailed := cfg.Failure != nil
		_ = injectorFailed
	}
	return model.BlockResult{Block: plan.Coord}, lastErr
}

func executeChunk(ctx context.Context, cfg Config, plan *eval.BlockPlan, workerID int, queue model.QueueRecord) (model.BlockResult, error) {
	idx := plan.Coord.Index
	if cfg.Hooks.OnWorkerStart != nil {
		cfg.Hooks.OnWorkerStart(idx, workerID)
	}
	machine := NewMachine(idx)
	queue.BlockIndex = idx
	queue.BufferSamples = plan.Coord.BufferSamples

	saveState := func(stage, state, detail string) {
		if cfg.Store == nil {
			return
		}
		st := machine.States[stage]
		_ = cfg.Store.SaveState(ctx, idx, st)
	}

	if err := machine.Transition(StageDecode, StateProcessing, "copy deterministic sample range"); err != nil {
		return model.BlockResult{Block: plan.Coord}, err
	}
	saveState(StageDecode, StateProcessing, "")
	buf, err := eval.SliceRange(cfg.Sequence.Original, cfg.Sequence.Reconstructed, cfg.Manifest, plan.Origin, plan.End)
	if err != nil {
		_ = machine.Transition(StageDecode, StateFailed, err.Error())
		saveState(StageDecode, StateFailed, err.Error())
		return model.BlockResult{Block: plan.Coord, States: machine.StageStates()}, err
	}
	if err := machine.Transition(StageDecode, StateCommitted, ""); err != nil {
		return model.BlockResult{Block: plan.Coord}, err
	}

	if err := machine.Transition(StageEvaluate, StateProcessing, ""); err != nil {
		return model.BlockResult{Block: plan.Coord}, err
	}
	saveState(StageEvaluate, StateProcessing, "")
	if err := cfg.Failure.BeforeEvaluate(idx); err != nil {
		_ = machine.Transition(StageEvaluate, StateFailed, "injected")
		saveState(StageEvaluate, StateFailed, err.Error())
		return model.BlockResult{Block: plan.Coord, States: machine.StageStates()}, err
	}
	result, err := eval.EvaluateRange(cfg.Manifest, cfg.Params, buf, plan.Coord.FirstFrame, plan.Coord.LastFrame, cfg.Quant)
	if err != nil {
		_ = machine.Transition(StageEvaluate, StateFailed, err.Error())
		saveState(StageEvaluate, StateFailed, err.Error())
		return model.BlockResult{Block: plan.Coord, States: machine.StageStates()}, err
	}
	if cfg.Truth != nil {
		for i := range result.Frames {
			f := result.Frames[i].Frame
			if f < len(cfg.Truth.Frames) {
				truthFrame := cfg.Truth.Frames[f]
				result.Frames[i].PulseExpectedSample = truthFrame.PulseSample
				result.Frames[i].PulseMatch = result.Frames[i].PulseObservedSample == truthFrame.PulseSample
			}
		}
	}
	if err := machine.Transition(StageEvaluate, StateCommitted, ""); err != nil {
		return model.BlockResult{Block: plan.Coord}, err
	}

	if err := machine.Transition(StageBound, StateProcessing, ""); err != nil {
		return model.BlockResult{Block: plan.Coord}, err
	}
	saveState(StageBound, StateProcessing, "")
	for _, fr := range result.Frames {
		if err := boundCheck(fr); err != nil {
			_ = machine.Transition(StageBound, StateFailed, err.Error())
			saveState(StageBound, StateFailed, err.Error())
			return model.BlockResult{Block: plan.Coord, States: machine.StageStates()}, err
		}
	}
	if err := machine.Transition(StageBound, StateCommitted, ""); err != nil {
		return model.BlockResult{Block: plan.Coord}, err
	}

	if err := machine.Transition(StageCheckpoint, StateProcessing, ""); err != nil {
		return model.BlockResult{Block: plan.Coord}, err
	}
	saveState(StageCheckpoint, StateProcessing, "")

	identity, err := BuildIdentity(cfg.Manifest, cfg.Sequence, cfg.IdentityTruthFP(), cfg.Params)
	if err != nil {
		return model.BlockResult{Block: plan.Coord}, err
	}
	start := plan.Origin * cfg.Manifest.Channels
	end := plan.End * cfg.Manifest.Channels
	block := model.BlockResult{
		Block:            plan.Coord,
		Manifest:         cfg.Manifest,
		EvalParams:       cfg.Params,
		ContentHash:      mustHash(cfg.Sequence.Original[start:end]),
		ParamVersion:     model.ParamSpecVersion,
		ParamHash:        identity.ParamHash,
		AlgorithmVersion: model.AlgorithmVersion,
		Frames:           result.Frames,
		Queue:            queue,
	}
	block.BlockHash = mustHash(struct {
		Coord  model.BlockCoord
		Frames []model.FrameReport
		Param  string
	}{plan.Coord, result.Frames, identity.ParamHash})
	if cfg.PersistSamples {
		block.Original = append([]int32(nil), cfg.Sequence.Original[start:end]...)
		block.Reconstructed = append([]int32(nil), cfg.Sequence.Reconstructed[start:end]...)
		block.ReconSummary = summarizeSamples(block.Reconstructed)
	}
	if err := machine.Transition(StageCheckpoint, StateCommitted, ""); err != nil {
		return model.BlockResult{Block: plan.Coord}, err
	}
	block.States = machine.StageStates()
	return block, nil
}

type identityTruthFPConfig interface {
	IdentityTruthFP() string
}

func (c Config) IdentityTruthFP() string {
	if c.Truth != nil {
		return c.Truth.Fingerprint
	}
	return ""
}

func boundCheck(fr model.FrameReport) error {
	return bound.CheckFrame(fr.Frame, fr.SNRDBFixed, fr.SNRInfinite, fr.NoiseFloorStdFixed, fr.Bound)
}

func mustHash(v any) string {
	h, err := model.HashCanonical(v)
	if err != nil {
		return ""
	}
	return h
}

func summarizeSamples(samples []int32) model.SampleSummary {
	sum := model.SampleSummary{Count: len(samples), SHA256: mustHash(samples)}
	if len(samples) > 0 {
		sum.Min = samples[0]
		sum.Max = samples[0]
	}
	for _, v := range samples {
		i := int64(v)
		sum.Sum += i
		sum.SumSq += i * i
		if v < sum.Min {
			sum.Min = v
		}
		if v > sum.Max {
			sum.Max = v
		}
	}
	return sum
}

func buildReport(ctx context.Context, cfg Config, existing map[int]model.BlockResult, outcomes map[int]outcome, commitOrder []int) (*model.Report, error) {
	var blocks []model.BlockResult
	if cfg.Store != nil {
		stored, err := cfg.Store.ListBlocks(ctx)
		if err != nil {
			return nil, err
		}
		blocks = stored
	} else {
		for _, out := range outcomes {
			if out.err == nil {
				blocks = append(blocks, out.block)
			}
		}
		sort.Slice(blocks, func(i, j int) bool { return blocks[i].Block.Index < blocks[j].Block.Index })
	}
	frames := []model.FrameReport{}
	queue := []model.QueueRecord{}
	for _, b := range blocks {
		frames = append(frames, b.Frames...)
		queue = append(queue, b.Queue)
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].Frame < frames[j].Frame })
	identity, _ := BuildIdentity(cfg.Manifest, cfg.Sequence, cfg.IdentityTruthFP(), cfg.Params)
	report := &model.Report{
		Identity: identity, Manifest: cfg.Manifest, EvalParams: cfg.Params, Budget: cfg.Budget,
		Blocks: blocks, Frames: frames, CommitOrder: commitOrder, Queue: queue,
		Truncations: eval.TruncationLog(), GeneratedUnixNano: time.Now().UnixNano(),
	}
	return report, nil
}
