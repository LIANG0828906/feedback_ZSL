package quantforensics

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

type BlockOutcome struct {
	Block          Block                 `json:"block"`
	ContentHash    string                `json:"contentHash"`
	Load           *LoadOutput           `json:"load,omitempty"`
	Quant          QuantOutput           `json:"quant"`
	Reconstruction ReconOutput           `json:"reconstruction"`
	Evaluation     BlockEvaluation       `json:"evaluation"`
	CacheKeys      map[BlockStage]string `json:"cacheKeys"`
	Attempts       int                   `json:"attempts"`
	ReusedStages   []BlockStage          `json:"reusedStages,omitempty"`
}

type LoadOutput struct {
	Block       Block  `json:"block"`
	ContentHash string `json:"contentHash"`
}

type ReconOutput struct {
	BlockID string `json:"blockId"`
	Pixels  []RGB  `json:"pixels"`
}

type StageError struct {
	Stage BlockStage
	Err   error
}

func (e *StageError) Error() string { return string(e.Stage) + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

type QueueWait struct {
	BlockID     string `json:"blockId"`
	Reason      string `json:"reason"`
	WaitedNanos int64  `json:"waitedNanos"`
}

type CommitRef struct {
	Seq      int64      `json:"seq"`
	BlockID  string     `json:"blockId"`
	Stage    BlockStage `json:"stage"`
	OrderKey int        `json:"orderKey"`
	CacheKey string     `json:"cacheKey"`
}

type RunReport struct {
	Mode             string         `json:"mode"`
	Outcomes         []BlockOutcome `json:"outcomes"`
	CommitSequence   []CommitRef    `json:"commitSequence"`
	QueueWaits       []QueueWait    `json:"queueWaits,omitempty"`
	ComputedBlocks   []string       `json:"computedBlocks"`
	ReusedBlocks     []string       `json:"reusedBlocks"`
	FailedBlocks     []string       `json:"failedBlocks,omitempty"`
	Interrupted      bool           `json:"interrupted,omitempty"`
	StopAfterCommits int            `json:"stopAfterCommits,omitempty"`
	StorePath        string         `json:"storePath,omitempty"`
	MaxParallelism   int            `json:"maxParallelism"`
	PixelBufferCap   int            `json:"pixelBufferCap"`
	QueueBudget      int            `json:"queueBudget"`
}

type EngineOptions struct {
	Mode                 string
	Workers              int
	PixelBufferPerWorker int
	MaxPendingBlocks     int
	ReuseCheckpoints     bool
	StopAfterCommits     int
	Invalidation         *InvalidationReport
	PanelID              string
	StateMachine         *StateMachine
}

type Engine struct {
	Project *Project
	Store   CheckpointStore
	FSM     *StateMachine
	Graph   *DependencyGraph
	Options EngineOptions
	Faults  *FaultPolicy
}

type FaultPolicy struct {
	mu       sync.Mutex
	failOnce map[string]bool
	attempts map[string]int
}

func NewFaultPolicy(failQuantOnce ...string) *FaultPolicy {
	f := &FaultPolicy{failOnce: map[string]bool{}, attempts: map[string]int{}}
	for _, id := range failQuantOnce {
		f.failOnce[id] = true
	}
	return f
}

func (f *FaultPolicy) BeforeQuant(blockID string) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[blockID]++
	if f.failOnce[blockID] && f.attempts[blockID] == 1 {
		return &Error{Code: CodeFault, Field: "quantize:" + blockID, Message: "injected single-block quant failure"}
	}
	return nil
}

func (f *FaultPolicy) Attempts(blockID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[blockID]
}

func NewEngine(p *Project, store CheckpointStore, opts EngineOptions) *Engine {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.PixelBufferPerWorker <= 0 {
		opts.PixelBufferPerWorker = 256
	}
	if opts.MaxPendingBlocks <= 0 {
		opts.MaxPendingBlocks = opts.Workers * 2
	}
	if opts.Mode == "" {
		opts.Mode = "realtime"
	}
	sm := opts.StateMachine
	if sm == nil {
		sm = NewStateMachine()
	}
	return &Engine{Project: p, Store: store, FSM: sm, Graph: BuildDependencyGraph(p), Options: opts}
}

func (e *Engine) stageKeys(blockID string) map[BlockStage]string {
	return map[BlockStage]string{
		StageLoad:        e.Graph.Nodes[LoadNodeID(blockID)].CacheKey,
		StageQuantize:    e.Graph.Nodes[IndexNodeID(blockID)].CacheKey,
		StageReconstruct: e.Graph.Nodes[ReconNodeID(blockID)].CacheKey,
		StageVerify:      e.Graph.Nodes[VerifyNodeID(blockID)].CacheKey,
	}
}

type cachedStages struct {
	quant QuantOutput
	recon ReconOutput
	eval  BlockEvaluation
	has   map[BlockStage]bool
}

func (e *Engine) loadCache() (map[string]StageRecord, map[string]cachedStages, []BlockOutcome, error) {
	latest := map[string]StageRecord{}
	cached := map[string]cachedStages{}
	reusedOutcomes := []BlockOutcome{}
	if !e.Options.ReuseCheckpoints || e.Store == nil {
		return latest, cached, reusedOutcomes, nil
	}
	records, err := e.Store.Records(e.Project.ProjectID)
	if err != nil {
		return nil, nil, nil, err
	}
	latest = LatestStageRecords(records)
	affected := map[string]bool{}
	if e.Options.Invalidation != nil {
		for _, id := range e.Options.Invalidation.AffectedBlocks {
			affected[id] = true
		}
	}
	for _, b := range e.Project.Blocks {
		if affected[b.ID] {
			continue
		}
		cs := cachedStages{has: map[BlockStage]bool{}}
		keys := e.stageKeys(b.ID)
		for _, stage := range OrderedStages {
			r, ok := latest[b.ID+":"+string(stage)]
			if !ok {
				continue
			}
			if e.Options.Invalidation == nil && r.CacheKey != keys[stage] {
				continue
			}
			cs.has[stage] = true
			switch stage {
			case StageQuantize:
				if err := json.Unmarshal(r.OutputJSON, &cs.quant); err != nil {
					return nil, nil, nil, fmt.Errorf("decode quant checkpoint %s: %w", b.ID, err)
				}
			case StageReconstruct:
				if err := json.Unmarshal(r.OutputJSON, &cs.recon); err != nil {
					return nil, nil, nil, fmt.Errorf("decode recon checkpoint %s: %w", b.ID, err)
				}
			case StageVerify:
				if err := json.Unmarshal(r.OutputJSON, &cs.eval); err != nil {
					return nil, nil, nil, fmt.Errorf("decode verify checkpoint %s: %w", b.ID, err)
				}
			}
		}
		cached[b.ID] = cs
		if cs.has[StageQuantize] && cs.has[StageReconstruct] && cs.has[StageVerify] {
			out := BlockOutcome{Block: b, ContentHash: cs.quant.ContentHash, Quant: cs.quant, Reconstruction: cs.recon, Evaluation: cs.eval, CacheKeys: keys, ReusedStages: []BlockStage{StageLoad, StageQuantize, StageReconstruct, StageVerify}}
			if load, ok := latest[b.ID+":"+string(StageLoad)]; ok {
				var lo LoadOutput
				_ = json.Unmarshal(load.OutputJSON, &lo)
				out.Load = &lo
			}
			reusedOutcomes = append(reusedOutcomes, out)
		}
	}
	return latest, cached, reusedOutcomes, nil
}

type job struct {
	index int
	block Block
	cache cachedStages
}

type envelope struct {
	index   int
	outcome BlockOutcome
	err     error
}

func (e *Engine) Run() (*RunReport, error) {
	if e.Store != nil {
		if err := e.Store.Init(); err != nil {
			return nil, err
		}
	}
	if e.Options.PanelID != "" {
		e.FSM.InitPanel(e.Options.PanelID)
		_ = e.FSM.TransitionPanel(e.Options.PanelID, StateRunning)
	}
	latest, cached, reused, err := e.loadCache()
	if err != nil {
		return nil, err
	}
	reusedByBlock := map[string]BlockOutcome{}
	for _, o := range reused {
		reusedByBlock[o.Block.ID] = o
	}

	report := &RunReport{Mode: e.Options.Mode, MaxParallelism: e.Options.Workers, PixelBufferCap: e.Options.PixelBufferPerWorker, QueueBudget: e.Options.MaxPendingBlocks}
	if e.Store != nil {
		report.StorePath = e.Store.Path()
	}

	jobs := make(chan job, e.Options.MaxPendingBlocks)
	results := make(chan envelope, len(e.Project.Blocks))
	buffers := make(chan [][3]int, e.Options.Workers)
	for i := 0; i < e.Options.Workers; i++ {
		buffers <- make([][3]int, 0, e.Options.PixelBufferPerWorker)
	}
	var wg sync.WaitGroup
	for w := 0; w < e.Options.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errBuf := <-buffers
			defer func() { buffers <- errBuf }()
			for j := range jobs {
				if j.block.W*j.block.H > cap(errBuf) {
					results <- envelope{index: j.index, err: &Error{Code: CodeBudget, Field: "pixelBuffer", Message: fmt.Sprintf("block pixels %d exceed per-worker buffer cap %d", j.block.W*j.block.H, cap(errBuf))}}
					continue
				}
				out, err := e.computeBlock(j.block, j.cache, errBuf)
				if err != nil {
					results <- envelope{index: j.index, err: err}
					continue
				}
				results <- envelope{index: j.index, outcome: out}
			}
		}()
	}

	for i, b := range e.Project.Blocks {
		if _, ok := reusedByBlock[b.ID]; ok {
			continue
		}
		cs := cached[b.ID]
		if len(jobs) >= cap(jobs) {
			start := time.Now()
			jobs <- job{index: i, block: b, cache: cs}
			report.QueueWaits = append(report.QueueWaits, QueueWait{BlockID: b.ID, Reason: "pending_block_queue_budget_exhausted", WaitedNanos: time.Since(start).Nanoseconds()})
		} else {
			jobs <- job{index: i, block: b, cache: cs}
		}
	}
	close(jobs)
	wg.Wait()
	close(results)

	outcomes := make([]BlockOutcome, len(e.Project.Blocks))
	errs := map[string]error{}
	for env := range results {
		b := e.Project.Blocks[env.index]
		if env.err != nil {
			errs[b.ID] = env.err
			continue
		}
		outcomes[env.index] = env.outcome
	}

	commits := 0
	for i, b := range e.Project.Blocks {
		if existing, ok := reusedByBlock[b.ID]; ok {
			outcomes[i] = existing
			report.ReusedBlocks = append(report.ReusedBlocks, b.ID)
			e.restoreCommitted(b.ID)
			continue
		}
		if runErr, failed := errs[b.ID]; failed {
			stage := StageQuantize
			var se *StageError
			if asError(runErr, &se) {
				stage = se.Stage
			}
			_ = e.FSM.TransitionBlock(b.ID, stage, StateRunning)
			_ = e.FSM.TransitionBlock(b.ID, stage, StateFailed)
			report.FailedBlocks = append(report.FailedBlocks, b.ID)
			continue
		}
		out := outcomes[i]
		out.CacheKeys = e.stageKeys(b.ID)
		e.commitOutcome(b, &out, latest, &commits, report)
		outcomes[i] = out
		if report.Interrupted {
			break
		}
	}

	for _, o := range outcomes {
		if o.Block.ID != "" {
			report.Outcomes = append(report.Outcomes, o)
		}
	}
	sort.Strings(report.FailedBlocks)
	if e.Store != nil {
		records, err := e.Store.Records(e.Project.ProjectID)
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			report.CommitSequence = append(report.CommitSequence, CommitRef{Seq: r.Seq, BlockID: r.BlockID, Stage: r.Stage, OrderKey: r.OrderKey, CacheKey: r.CacheKey})
		}
	}
	if e.Options.PanelID != "" {
		if len(report.FailedBlocks) > 0 {
			_ = e.FSM.TransitionPanel(e.Options.PanelID, StateFailed)
		} else if !report.Interrupted {
			_ = e.FSM.TransitionPanel(e.Options.PanelID, StateCommitted)
		}
	}
	return report, nil
}

func asError(err error, target **StageError) bool {
	for err != nil {
		if se, ok := err.(*StageError); ok {
			*target = se
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

func (e *Engine) restoreCommitted(blockID string) {
	for _, stage := range OrderedStages {
		if e.FSM.BlockState(blockID, stage) == StatePending {
			_ = e.FSM.TransitionBlock(blockID, stage, StateRunning)
			_ = e.FSM.TransitionBlock(blockID, stage, StateCommitted)
		}
	}
}

func (e *Engine) computeBlock(b Block, cache cachedStages, errBuf [][3]int) (BlockOutcome, error) {
	im, ok := e.image(b.ImageID)
	if !ok {
		return BlockOutcome{}, &StageError{Stage: StageLoad, Err: schemaError("$.frames", "image %q not found", b.ImageID)}
	}
	contentHash := BlockContentHash(im, b)
	out := BlockOutcome{Block: b, ContentHash: contentHash, CacheKeys: e.stageKeys(b.ID)}
	out.Load = &LoadOutput{Block: b, ContentHash: contentHash}

	if cache.has[StageQuantize] {
		out.Quant = cache.quant
		out.ReusedStages = append(out.ReusedStages, StageQuantize)
	} else {
		if err := e.Faults.BeforeQuant(b.ID); err != nil {
			return BlockOutcome{}, &StageError{Stage: StageQuantize, Err: err}
		}
		q, err := QuantizeBlockWithBuffer(im, b, e.Project.Palette, e.Project.Params, errBuf)
		if err != nil {
			return BlockOutcome{}, &StageError{Stage: StageQuantize, Err: err}
		}
		out.Quant = q
	}
	if cache.has[StageReconstruct] {
		out.Reconstruction = cache.recon
		out.ReusedStages = append(out.ReusedStages, StageReconstruct)
	} else {
		pixels, err := ReconstructBlock(out.Quant.Indices, e.Project.Palette, b.W, b.H)
		if err != nil {
			return BlockOutcome{}, &StageError{Stage: StageReconstruct, Err: err}
		}
		out.Reconstruction = ReconOutput{BlockID: b.ID, Pixels: pixels}
	}
	if cache.has[StageVerify] {
		out.Evaluation = cache.eval
		out.ReusedStages = append(out.ReusedStages, StageVerify)
	} else {
		out.Evaluation = EvaluateBlock(im, b, out.Reconstruction.Pixels, e.Project.Params.Metric, e.Project.Tolerance)
	}
	return out, nil
}

func (e *Engine) image(id string) (Image, bool) {
	for _, im := range e.Project.Images {
		if im.ID == id {
			return im, true
		}
	}
	return Image{}, false
}

func (e *Engine) commitOutcome(b Block, out *BlockOutcome, latest map[string]StageRecord, commits *int, report *RunReport) {
	orderBase := b.Index * len(OrderedStages)
	invalidStages := map[BlockStage]bool{}
	if e.Options.Invalidation != nil {
		for _, item := range e.Options.Invalidation.Items {
			if item.BlockID == b.ID {
				invalidStages[item.Stage] = true
			}
		}
	}
	for stageIndex, stage := range OrderedStages {
		if e.Options.Invalidation != nil {
			if !invalidStages[stage] {
				e.restoreStageCommitted(b.ID, stage)
				out.ReusedStages = append(out.ReusedStages, stage)
				continue
			}
		} else {
			if r, ok := latest[b.ID+":"+string(stage)]; ok && r.CacheKey == out.CacheKeys[stage] && e.Options.ReuseCheckpoints {
				e.restoreStageCommitted(b.ID, stage)
				out.ReusedStages = append(out.ReusedStages, stage)
				continue
			}
		}
		_ = e.FSM.TransitionBlock(b.ID, stage, StateRunning)
		summary, output, contentHash := e.stageCheckpointPayload(*out, stage)
		rec := StageRecord{
			ProjectID: e.Project.ProjectID, BlockID: b.ID, Stage: stage, State: string(StateCommitted),
			OrderKey: orderBase + stageIndex, OutputSummary: summary, CacheKey: out.CacheKeys[stage],
			ContentHash: contentHash, ParamVersion: e.Project.Params.ParamVersion, AlgoVersion: AlgoVersion,
			OutputJSON: output,
		}
		if e.Store != nil {
			if _, err := e.Store.CommitStage(rec); err != nil {
				_ = e.FSM.TransitionBlock(b.ID, stage, StateFailed)
				report.FailedBlocks = append(report.FailedBlocks, b.ID+":checkpoint")
				return
			}
		}
		_ = e.FSM.TransitionBlock(b.ID, stage, StateCommitted)
		*commits++
		if stage == StageQuantize {
			report.ComputedBlocks = append(report.ComputedBlocks, b.ID)
		}
		if e.Options.StopAfterCommits > 0 && *commits >= e.Options.StopAfterCommits {
			report.Interrupted = true
			report.StopAfterCommits = e.Options.StopAfterCommits
			return
		}
	}
}

func (e *Engine) restoreStageCommitted(blockID string, stage BlockStage) {
	if e.FSM.BlockState(blockID, stage) == StatePending {
		_ = e.FSM.TransitionBlock(blockID, stage, StateRunning)
		_ = e.FSM.TransitionBlock(blockID, stage, StateCommitted)
	}
}

func (e *Engine) stageCheckpointPayload(out BlockOutcome, stage BlockStage) (string, []byte, string) {
	switch stage {
	case StageLoad:
		b, _ := json.Marshal(out.Load)
		return "load contentHash=" + shortHash(out.ContentHash), b, out.ContentHash
	case StageQuantize:
		b, _ := json.Marshal(out.Quant)
		return fmt.Sprintf("indices=%d hash=%s", len(out.Quant.Indices), shortHash(HashHex(b))), b, out.ContentHash
	case StageReconstruct:
		b, _ := json.Marshal(out.Reconstruction)
		return "pixels=" + fmt.Sprint(len(out.Reconstruction.Pixels)) + " hash=" + shortHash(HashHex(b)), b, out.ContentHash
	default:
		b, _ := json.Marshal(out.Evaluation)
		return fmt.Sprintf("passed=%v weightedDiff=%d maxR=%d maxG=%d maxB=%d", out.Evaluation.Passed, out.Evaluation.WeightedColorDiff, out.Evaluation.MaxDeviation[0], out.Evaluation.MaxDeviation[1], out.Evaluation.MaxDeviation[2]), b, out.ContentHash
	}
}

func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}
