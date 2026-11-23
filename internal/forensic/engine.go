package forensic

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var ErrCancelled = errors.New("analysis cancelled")

type Control struct {
	mu      sync.Mutex
	paused  bool
	cancel  chan struct{}
	changed chan struct{}
	once    sync.Once
}

func NewControl() *Control {
	return &Control{cancel: make(chan struct{}), changed: make(chan struct{})}
}

func (c *Control) Pause() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.paused = true
	c.signalLocked()
	c.mu.Unlock()
}

func (c *Control) Resume() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.paused = false
	c.signalLocked()
	c.mu.Unlock()
}

func (c *Control) Cancel() {
	if c == nil {
		return
	}
	c.once.Do(func() { close(c.cancel) })
	c.mu.Lock()
	c.signalLocked()
	c.mu.Unlock()
}

func (c *Control) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Control) Wait(ctx context.Context) error {
	if c == nil {
		return ctx.Err()
	}
	for {
		c.mu.Lock()
		paused := c.paused
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-c.cancel:
			return ErrCancelled
		default:
		}
		if !paused {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.cancel:
			return ErrCancelled
		case <-changed:
		}
	}
}

type QueueEvent struct {
	BlockIndex int    `json:"blockIndex"`
	Stage      string `json:"stage"`
	Reason     string `json:"reason"`
}

type RunOptions struct {
	ScopeBlocks        []int
	CommitHook         func(BlockState) error
	HeatmapPath        string
	FailOnHashMismatch bool
}

type RunResult struct {
	ImageID              string          `json:"imageId"`
	Params               Params          `json:"params"`
	ParamsVersion        string          `json:"paramsVersion"`
	AlgorithmVersion     string          `json:"algorithmVersion"`
	BlockSize            int             `json:"blockSize"`
	Chunks               []Chunk         `json:"chunks"`
	States               []BlockState    `json:"states"`
	Summary              Summary         `json:"summary"`
	Invalidations        []Invalidation  `json:"invalidations"`
	Reuses               []ReuseRecord   `json:"reuses"`
	AffectedBlocks       []int           `json:"affectedBlocks"`
	QueueEvents          []QueueEvent    `json:"queueEvents"`
	DependencyGraph      DependencyGraph `json:"dependencyGraph"`
	HeatmapKey           string          `json:"heatmapKey"`
	HeatmapSHA256        string          `json:"heatmapSha256"`
	HeatmapReused        bool            `json:"heatmapReused"`
	SnapshotResetReason  string          `json:"snapshotResetReason,omitempty"`
	Truth                []BlockTruth    `json:"truth"`
	TruthConfusion       TruthConfusion  `json:"truthConfusion"`
	TruthAgreement       float64         `json:"truthAgreement"`
	TruthWithinTolerance bool            `json:"truthWithinTolerance"`
	CommitSequence       []int           `json:"commitSequence"`
	CommitLedger         []int           `json:"commitLedger"`
	Completed            bool            `json:"completed"`
	HeatmapPNG           []byte          `json:"-"`
	HeatmapRGBA          []byte          `json:"-"`
	StartedAt            string          `json:"startedAt"`
	FinishedAt           string          `json:"finishedAt"`
}

type Engine struct {
	Image   *Image
	Store   *Store
	session bool
}

func NewEngine(img *Image, store *Store) *Engine {
	return &Engine{Image: img, Store: store}
}

func (e *Engine) VerifyPersistedHashes(ctx context.Context) error {
	if e == nil || e.Image == nil || e.Store == nil {
		return ParamError("engine", "image and checkpoint store are required")
	}
	snap, states, err := e.Store.LoadSnapshot(ctx, e.Image.ID)
	if err != nil {
		return err
	}
	if snap == nil {
		return ParamError("snapshot", "no checkpoint exists for this image")
	}
	chunks, err := MakeChunks(e.Image.Width, e.Image.Height, snap.BlockSize)
	if err != nil {
		return err
	}
	expected := make(map[int]string, len(states))
	for _, state := range states {
		expected[state.BlockIndex] = state.ContentDigest
	}
	return VerifyContentHashes(e.Image, chunks, expected)
}

type blockPlan struct {
	chunk       Chunk
	old         *BlockState
	needHash    bool
	needStats   bool
	forceCommit bool
}

type workResult struct {
	state BlockState
	err   error
}

type runAccumulator struct {
	mu      sync.Mutex
	events  []QueueEvent
	seen    map[string]bool
	limiter chan struct{}
}

func (a *runAccumulator) record(block int, stage, reason string) {
	key := fmt.Sprintf("%d/%s/%s", block, stage, reason)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen[key] {
		return
	}
	a.seen[key] = true
	a.events = append(a.events, QueueEvent{BlockIndex: block, Stage: stage, Reason: reason})
}

func (a *runAccumulator) eventsCopy() []QueueEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]QueueEvent(nil), a.events...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].BlockIndex != out[j].BlockIndex {
			return out[i].BlockIndex < out[j].BlockIndex
		}
		return out[i].Stage < out[j].Stage
	})
	return out
}

func (e *Engine) Run(ctx context.Context, p Params, opt RunOptions, ctl *Control) (RunResult, error) {
	started := time.Now().UTC().Format(time.RFC3339Nano)
	if e == nil || e.Image == nil || e.Store == nil {
		return RunResult{StartedAt: started}, ParamError("engine", "image and checkpoint store are required")
	}
	if err := p.Validate(e.Image); err != nil {
		return RunResult{StartedAt: started}, err
	}
	chunks, err := MakeChunks(e.Image.Width, e.Image.Height, p.BlockSize)
	if err != nil {
		return RunResult{StartedAt: started}, err
	}
	if err := validateScope(opt.ScopeBlocks, len(chunks)); err != nil {
		return RunResult{StartedAt: started}, err
	}
	pv := ParamsVersion(e.Image, p)
	digests := make([]string, len(chunks))
	for i, chunk := range chunks {
		digests[i] = e.Image.BlockDigest(chunk)
	}

	snap, oldStates, err := e.Store.LoadSnapshot(ctx, e.Image.ID)
	if err != nil {
		return RunResult{StartedAt: started}, err
	}
	oldMap := make(map[int]BlockState, len(oldStates))
	for _, s := range oldStates {
		oldMap[s.BlockIndex] = s
	}
	resetReason := e.snapshotResetReason(snap, oldStates, chunks, digests, pv, p)
	if resetReason != "" && opt.FailOnHashMismatch && resetReason == "content_hash_mismatch" {
		return RunResult{StartedAt: started}, &Error{
			Code: CodeHash, ImageID: e.Image.ID, Block: firstContentMismatch(oldStates, chunks, digests),
			Stage: StageHash, Message: "persisted content hash does not match the current image",
		}
	}
	if resetReason != "" {
		oldMap = map[int]BlockState{}
		oldStates = nil
		snap = nil
		if err := e.Store.ResetImage(ctx, Snapshot{
			ImageID: e.Image.ID, ParamsVersion: pv, AlgorithmVersion: AlgorithmVersion,
			Width: e.Image.Width, Height: e.Image.Height, Channels: e.Image.Channels,
			BlockSize: p.BlockSize, Complete: false,
		}); err != nil {
			return RunResult{StartedAt: started}, err
		}
	} else {
		if err := e.Store.UpsertSnapshot(ctx, Snapshot{
			ImageID: e.Image.ID, ParamsVersion: pv, AlgorithmVersion: AlgorithmVersion,
			Width: e.Image.Width, Height: e.Image.Height, Channels: e.Image.Channels,
			BlockSize: p.BlockSize, Complete: false,
		}); err != nil {
			return RunResult{StartedAt: started}, err
		}
	}
	e.session = true

	result := RunResult{
		ImageID: e.Image.ID, Params: p, ParamsVersion: pv, AlgorithmVersion: AlgorithmVersion,
		BlockSize: p.BlockSize, Chunks: chunks, SnapshotResetReason: resetReason,
		StartedAt: started,
	}
	plans, thresholdOnly := buildPlan(chunks, oldMap, digests, p)
	if len(opt.ScopeBlocks) > 0 {
		if !allOldStatesPresent(oldMap, len(chunks)) {
			return RunResult{StartedAt: started}, ParamError("scopeBlocks", "targeted retry requires a complete checkpoint snapshot")
		}
		if pv != snapshotParamsVersion(oldStates) && len(oldStates) > 0 {
			return RunResult{StartedAt: started}, ParamError("scopeBlocks", "targeted retry requires unchanged hash, window and threshold parameters")
		}
		plans, thresholdOnly = applyScopePlans(plans, chunks, oldMap, opt.ScopeBlocks)
	}

	acc := &runAccumulator{seen: map[string]bool{}}
	maxChunkBytes := maxBlockPixelBytes(e.Image, p.BlockSize)
	limiterCapacity := p.PixelBufferLimit / maxChunkBytes
	if limiterCapacity < 1 {
		return RunResult{StartedAt: started}, ParamError("pixelBufferLimit", "must be at least one block of pixel bytes")
	}
	acc.limiter = make(chan struct{}, limiterCapacity)

	states := make([]BlockState, len(chunks))
	reused := make(map[int]bool)
	var commitOrder []int
	commitLedger := []int(nil)
	allPresent := allOldStatesPresent(oldMap, len(chunks))

	if thresholdOnly && allPresent && len(opt.ScopeBlocks) == 0 {
		if err := e.thresholdIncremental(ctx, chunks, oldMap, p, pv, opt, &result, states, reused, &commitOrder, ctl); err != nil {
			result.States = presentStates(states)
			return result, err
		}
	} else {
		if err := e.recompute(ctx, chunks, digests, plans, p, pv, opt, ctl, acc, &result, states, reused, &commitOrder, &commitLedger); err != nil {
			result.States = presentStates(states)
			result.QueueEvents = acc.eventsCopy()
			return result, err
		}
	}

	finalStates := presentStates(states)
	sort.Slice(finalStates, func(i, j int) bool { return finalStates[i].BlockIndex < finalStates[j].BlockIndex })
	for i := range finalStates {
		if finalStates[i].BlockIndex != i {
			return result, fmt.Errorf("internal snapshot gap at block %d", i)
		}
	}
	heatmapRGBA, heatmapPNG, heatmapKey, err := BuildHeatmap(e.Image, chunks, finalStates)
	if err != nil {
		return result, err
	}
	summary := BuildSummary(finalStates, reused)
	truth := TruthBlocks(e.Image, chunks)
	confusion, agreement := EvaluateTruth(finalStates, truth)
	summary.TruthAgreement = agreement
	summary.TruthConfusion = confusion
	summary.SummaryDigest = summaryDigestWithTruth(finalStates, agreement, confusion)

	result.States = finalStates
	result.Summary = summary
	result.Truth = truth
	result.TruthConfusion = confusion
	result.TruthAgreement = agreement
	result.TruthWithinTolerance = agreement >= 1-e.Image.Entry.MaskTolerance
	result.HeatmapKey = heatmapKey
	result.HeatmapSHA256 = sha256Hex(heatmapRGBA)
	result.HeatmapPNG = heatmapPNG
	result.HeatmapRGBA = heatmapRGBA
	result.CommitSequence = summary.CommitSequence
	result.CommitLedger = commitLedger
	result.QueueEvents = acc.eventsCopy()
	result.DependencyGraph = BuildDependencyGraph(finalStates, heatmapKey)
	result.Completed = true
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if snap != nil && snap.Complete && snap.HeatmapKey == heatmapKey && snap.SummaryDigest == summary.SummaryDigest {
		result.HeatmapReused = true
	}
	if len(result.Invalidations) > 0 {
		result.HeatmapReused = false
	}
	if len(opt.HeatmapPath) > 0 {
		if err := os.MkdirAll(filepath.Dir(opt.HeatmapPath), 0o755); err != nil {
			return result, DecodeError("create heatmap output directory", err)
		}
		if err := atomicWrite(opt.HeatmapPath, heatmapPNG, 0o644); err != nil {
			return result, DecodeError("write heatmap", err)
		}
	}
	if err := e.Store.MarkComplete(ctx, e.Image.ID, heatmapKey, summary.SummaryDigest); err != nil {
		return result, err
	}
	if ledger, err := e.Store.CommitOrder(ctx, e.Image.ID); err == nil {
		result.CommitLedger = ledger
	}
	return result, nil
}

func (e *Engine) snapshotResetReason(snap *Snapshot, states []BlockState, chunks []Chunk, digests []string, pv string, p Params) string {
	if snap == nil {
		return "cache_missing"
	}
	if snap.Width != e.Image.Width || snap.Height != e.Image.Height || snap.Channels != e.Image.Channels || snap.BlockSize != p.BlockSize {
		return "block_layout_mismatch"
	}
	if !e.session && snap.ParamsVersion != pv {
		return "params_version_mismatch"
	}
	if snap.AlgorithmVersion != AlgorithmVersion {
		return "algorithm_version_mismatch"
	}
	for _, s := range states {
		if s.AlgorithmVersion != AlgorithmVersion {
			return "algorithm_version_mismatch"
		}
		if s.ParamsVersion != snap.ParamsVersion {
			return "mixed_params_versions"
		}
		if s.BlockIndex < 0 || s.BlockIndex >= len(chunks) {
			return "block_layout_mismatch"
		}
		c := chunks[s.BlockIndex]
		if s.X0 != c.X0 || s.Y0 != c.Y0 || s.X1 != c.X1 || s.Y1 != c.Y1 {
			return "block_layout_mismatch"
		}
		if s.ContentDigest != digests[s.BlockIndex] {
			return "content_hash_mismatch"
		}
	}
	return ""
}

func buildPlan(chunks []Chunk, oldMap map[int]BlockState, digests []string, p Params) ([]blockPlan, bool) {
	plans := make([]blockPlan, len(chunks))
	thresholdOnly := len(oldMap) == len(chunks) && len(chunks) > 0
	for _, chunk := range chunks {
		plans[chunk.Index].chunk = chunk
		old, ok := oldMap[chunk.Index]
		if !ok {
			plans[chunk.Index].needHash = true
			plans[chunk.Index].needStats = true
			thresholdOnly = false
			continue
		}
		oldCopy := old
		plans[chunk.Index].old = &oldCopy
		newHashKey := HashKey(digests[chunk.Index], p)
		newStatsKey := StatsKey(newHashKey, p)
		newVerdictKey := VerdictKey(newStatsKey, p.Threshold)
		if old.HashKey != newHashKey {
			plans[chunk.Index].needHash = true
			plans[chunk.Index].needStats = true
			thresholdOnly = false
			continue
		}
		if old.StatsKey != newStatsKey {
			plans[chunk.Index].needStats = true
			thresholdOnly = false
			continue
		}
		if old.VerdictKey == newVerdictKey {
			thresholdOnly = false
		}
	}
	return plans, thresholdOnly
}

func applyScopePlans(plans []blockPlan, chunks []Chunk, oldMap map[int]BlockState, scope []int) ([]blockPlan, bool) {
	selected := map[int]bool{}
	for _, idx := range scope {
		selected[idx] = true
	}
	for _, chunk := range chunks {
		old, ok := oldMap[chunk.Index]
		if ok {
			copy := old
			plans[chunk.Index].old = &copy
		}
		plans[chunk.Index].needHash = false
		plans[chunk.Index].needStats = selected[chunk.Index]
		plans[chunk.Index].forceCommit = selected[chunk.Index]
	}
	return plans, false
}

func (e *Engine) recompute(ctx context.Context, chunks []Chunk, digests []string, plans []blockPlan,
	p Params, pv string, opt RunOptions, ctl *Control, acc *runAccumulator,
	result *RunResult, states []BlockState, reused map[int]bool, commitOrder, commitLedger *[]int) error {

	jobsToRun := make([]Chunk, 0)
	slots := make([]chan workResult, len(chunks))
	for _, chunk := range chunks {
		plan := plans[chunk.Index]
		if !plan.needHash && !plan.needStats && !plan.forceCommit && plan.old != nil {
			continue
		}
		jobsToRun = append(jobsToRun, chunk)
		slots[chunk.Index] = make(chan workResult, 1)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		if ctl == nil {
			return
		}
		select {
		case <-ctl.cancel:
			cancel()
		case <-runCtx.Done():
		}
	}()
	jobs := make(chan Chunk, p.QueueCapacity)
	go func() {
		defer close(jobs)
		for _, chunk := range jobsToRun {
			if err := waitControl(runCtx, ctl); err != nil {
				return
			}
			select {
			case jobs <- chunk:
			default:
				acc.record(chunk.Index, StageAnalysis, "queue_capacity_blocked")
				select {
				case jobs <- chunk:
				case <-runCtx.Done():
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < p.WorkerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range jobs {
				plan := plans[chunk.Index]
				if err := waitControl(runCtx, ctl); err != nil {
					slots[chunk.Index] <- workResult{err: err}
					continue
				}
				select {
				case acc.limiter <- struct{}{}:
				default:
					acc.record(chunk.Index, StageAnalysis, "pixel_buffer_limit_blocked")
					select {
					case acc.limiter <- struct{}{}:
					case <-runCtx.Done():
						slots[chunk.Index] <- workResult{err: runCtx.Err()}
						continue
					}
				}
				state, err := computeState(e.Image, chunk, digests[chunk.Index], plan.old, p, pv, plan.needHash, plan.needStats)
				<-acc.limiter
				slots[chunk.Index] <- workResult{state: state, err: err}
			}
		}()
	}
	go func() {
		wg.Wait()
	}()

	for _, chunk := range chunks {
		plan := plans[chunk.Index]
		if !plan.needHash && !plan.needStats && !plan.forceCommit && plan.old != nil {
			states[chunk.Index] = *plan.old
			reused[chunk.Index] = true
			recordReuse(result, chunk.Index, StageHash, "cache_key_match")
			recordReuse(result, chunk.Index, StageAnalysis, "cache_key_match")
			recordReuse(result, chunk.Index, StageSummary, "cache_key_match")
			continue
		}
		var wr workResult
		select {
		case wr = <-slots[chunk.Index]:
		case <-runCtx.Done():
			return cancelledError(runCtx, ctl)
		}
		if wr.err != nil {
			if errors.Is(wr.err, ErrCancelled) || errors.Is(wr.err, context.Canceled) {
				return ErrCancelled
			}
			return wr.err
		}
		if err := waitControl(runCtx, ctl); err != nil {
			return err
		}
		state := wr.state
		state.ImageID = e.Image.ID
		state.Finalize()
		reusedKeys := plan.old != nil && !plan.needStats && !plan.needHash
		if prior := plan.old; prior != nil && state.StateDigest != prior.StateDigest && (reusedKeys || plan.forceCommit) {
			return &Error{Code: CodeDiverge, ImageID: e.Image.ID, Block: chunk.Index, Stage: StageAnalysis,
				Message: "deterministic retry produced a different exact block result"}
		}
		states[chunk.Index] = state
		seq, err := e.Store.CommitBlock(ctx, state, commitStage(plan))
		if err != nil {
			return err
		}
		states[chunk.Index].CommitSeq = seq
		*commitOrder = append(*commitOrder, chunk.Index)
		*commitLedger = append(*commitLedger, chunk.Index)
		if opt.CommitHook != nil {
			if err := opt.CommitHook(states[chunk.Index]); err != nil {
				return err
			}
		}
		addPlanInvalidations(result, chunk.Index, plan)
		if !plan.needHash && plan.old != nil {
			recordReuse(result, chunk.Index, StageHash, "upstream_independent_reuse")
		}
	}
	if len(result.Invalidations) > 0 {
		addHeatmapInvalidation(result, "upstream_summary_changed")
	}
	return nil
}

func computeState(img *Image, chunk Chunk, digest string, old *BlockState, p Params, pv string, needHash, needStats bool) (BlockState, error) {
	var state BlockState
	if old != nil && !needHash && !needStats {
		state = *old
	} else if old != nil {
		state = *old
	}
	state.ImageID = img.ID
	state.BlockIndex = chunk.Index
	state.X0, state.Y0, state.X1, state.Y1 = chunk.X0, chunk.Y0, chunk.X1, chunk.Y1
	state.ContentDigest = digest
	state.ParamsVersion = pv
	state.AlgorithmVersion = AlgorithmVersion
	state.Threshold = p.Threshold
	state.Window = p.Window
	if needHash || state.ContentHash == "" {
		hash, err := BlockContentHash(img, chunk, p.HashAlgorithm)
		if err != nil {
			return BlockState{}, err
		}
		fp, err := Fingerprint(img, chunk, p.FingerprintGrid)
		if err != nil {
			return BlockState{}, err
		}
		state.ContentHash, state.Fingerprint = hash, fp
	}
	if needStats || state.StatsKey == "" {
		stats, err := AnalyzeBlock(img, chunk, p)
		if err != nil {
			return BlockState{}, err
		}
		state.Stats = stats
	}
	state.HashKey = HashKey(digest, p)
	state.StatsKey = StatsKey(state.HashKey, p)
	state.VerdictKey = VerdictKey(state.StatsKey, p.Threshold)
	state.SummaryKey = SummaryKey(state.VerdictKey)
	state.Tampered = state.Stats.TamperScore >= p.Threshold
	return state, nil
}

func (e *Engine) thresholdIncremental(ctx context.Context, chunks []Chunk, oldMap map[int]BlockState,
	p Params, pv string, opt RunOptions, result *RunResult,
	states []BlockState, reused map[int]bool, commitOrder *[]int, ctl *Control) error {

	summaryKeys := make(map[int]string, len(chunks))
	verdictKeys := make(map[int]string, len(chunks))
	for _, chunk := range chunks {
		if err := waitControl(ctx, ctl); err != nil {
			return err
		}
		old := oldMap[chunk.Index]
		state := old
		state.ImageID = e.Image.ID
		state.ParamsVersion = pv
		state.Threshold = p.Threshold
		state.Window = p.Window
		state.HashKey = HashKey(state.ContentDigest, p)
		state.StatsKey = StatsKey(state.HashKey, p)
		state.VerdictKey = VerdictKey(state.StatsKey, p.Threshold)
		state.SummaryKey = SummaryKey(state.VerdictKey)
		newVerdict := state.Stats.TamperScore >= p.Threshold
		flipped := newVerdict != state.Tampered
		state.Tampered = newVerdict
		state.Finalize()
		states[chunk.Index] = state
		verdictKeys[chunk.Index] = state.VerdictKey
		summaryKeys[chunk.Index] = state.SummaryKey
		if !flipped {
			reused[chunk.Index] = true
			recordReuse(result, chunk.Index, StageHash, "threshold_does_not_affect_hash")
			recordReuse(result, chunk.Index, StageAnalysis, "statistics_reused_verdict_unchanged")
			recordReuse(result, chunk.Index, StageSummary, "verdict_unchanged")
			continue
		}
		seq, err := e.Store.CommitBlock(ctx, state, "analysis")
		if err != nil {
			return err
		}
		states[chunk.Index].CommitSeq = seq
		*commitOrder = append(*commitOrder, chunk.Index)
		result.Invalidations = append(result.Invalidations,
			Invalidation{BlockIndex: chunk.Index, Stage: StageAnalysis, Reason: "threshold_verdict_flip", CacheKey: state.VerdictKey},
			Invalidation{BlockIndex: chunk.Index, Stage: StageSummary, Reason: "upstream_verdict_changed", CacheKey: state.SummaryKey},
		)
		result.AffectedBlocks = append(result.AffectedBlocks, chunk.Index)
		if opt.CommitHook != nil {
			if err := opt.CommitHook(states[chunk.Index]); err != nil {
				return err
			}
		}
	}
	if err := e.Store.PromoteMetadata(ctx, e.Image.ID, pv, p.Threshold, p.Window, verdictKeys, summaryKeys); err != nil {
		return err
	}
	addHeatmapInvalidation(result, "threshold_changed")
	return nil
}

func addPlanInvalidations(result *RunResult, block int, plan blockPlan) {
	if plan.old == nil {
		result.Invalidations = append(result.Invalidations,
			Invalidation{BlockIndex: block, Stage: StageHash, Reason: "cache_missing"},
			Invalidation{BlockIndex: block, Stage: StageAnalysis, Reason: "cache_missing"},
			Invalidation{BlockIndex: block, Stage: StageSummary, Reason: "cache_missing"},
		)
		result.AffectedBlocks = append(result.AffectedBlocks, block)
		return
	}
	if plan.forceCommit {
		result.Invalidations = append(result.Invalidations,
			Invalidation{BlockIndex: block, Stage: StageAnalysis, Reason: "block_retry_requested"},
			Invalidation{BlockIndex: block, Stage: StageSummary, Reason: "upstream_analysis_changed"},
		)
		result.AffectedBlocks = append(result.AffectedBlocks, block)
		return
	}
	if plan.needHash {
		result.Invalidations = append(result.Invalidations,
			Invalidation{BlockIndex: block, Stage: StageHash, Reason: "hash_parameters_or_content_changed"},
			Invalidation{BlockIndex: block, Stage: StageAnalysis, Reason: "upstream_hash_changed"},
			Invalidation{BlockIndex: block, Stage: StageSummary, Reason: "upstream_analysis_changed"},
		)
		result.AffectedBlocks = append(result.AffectedBlocks, block)
		return
	}
	if plan.needStats {
		result.Invalidations = append(result.Invalidations,
			Invalidation{BlockIndex: block, Stage: StageAnalysis, Reason: "window_parameters_changed"},
			Invalidation{BlockIndex: block, Stage: StageSummary, Reason: "upstream_analysis_changed"},
		)
		result.AffectedBlocks = append(result.AffectedBlocks, block)
		return
	}
}

func addHeatmapInvalidation(result *RunResult, reason string) {
	for _, inv := range result.Invalidations {
		if inv.Stage == StageHeatmap {
			return
		}
	}
	result.Invalidations = append(result.Invalidations, Invalidation{
		BlockIndex: -1, Stage: StageHeatmap, Reason: reason,
	})
}

func recordReuse(result *RunResult, block int, stage, reason string) {
	for _, r := range result.Reuses {
		if r.BlockIndex == block && r.Stage == stage {
			return
		}
	}
	result.Reuses = append(result.Reuses, ReuseRecord{BlockIndex: block, Stage: stage, Reason: reason})
}

func commitStage(plan blockPlan) string {
	switch {
	case plan.needHash:
		return "hash+analysis"
	case plan.needStats:
		return "analysis"
	default:
		return "retry"
	}
}

func validateScope(scope []int, count int) error {
	seen := map[int]bool{}
	for _, idx := range scope {
		if idx < 0 || idx >= count {
			return ParamError("scopeBlocks", fmt.Sprintf("block %d is outside [0,%d)", idx, count))
		}
		if seen[idx] {
			return ParamError("scopeBlocks", fmt.Sprintf("block %d is duplicated", idx))
		}
		seen[idx] = true
	}
	return nil
}

func allOldStatesPresent(oldMap map[int]BlockState, count int) bool {
	if len(oldMap) != count {
		return false
	}
	for i := 0; i < count; i++ {
		if _, ok := oldMap[i]; !ok {
			return false
		}
	}
	return true
}

func snapshotParamsVersion(states []BlockState) string {
	if len(states) == 0 {
		return ""
	}
	return states[0].ParamsVersion
}

func firstContentMismatch(states []BlockState, chunks []Chunk, digests []string) int {
	for _, s := range states {
		if s.BlockIndex >= 0 && s.BlockIndex < len(digests) && s.ContentDigest != digests[s.BlockIndex] {
			return s.BlockIndex
		}
	}
	for _, c := range chunks {
		if c.Index < len(digests) {
			return c.Index
		}
	}
	return 0
}

func presentStates(states []BlockState) []BlockState {
	out := make([]BlockState, 0, len(states))
	seen := map[int]bool{}
	for _, s := range states {
		if s.AlgorithmVersion == "" && s.BlockIndex == 0 && s.ContentDigest == "" {
			continue
		}
		if seen[s.BlockIndex] {
			continue
		}
		seen[s.BlockIndex] = true
		out = append(out, s)
	}
	return out
}

func waitControl(ctx context.Context, ctl *Control) error {
	if ctl == nil {
		return ctx.Err()
	}
	return ctl.Wait(ctx)
}

func cancelledError(ctx context.Context, ctl *Control) error {
	if ctl != nil {
		select {
		case <-ctl.cancel:
			return ErrCancelled
		default:
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrCancelled
	}
	return ErrCancelled
}

func summaryDigestWithTruth(states []BlockState, agreement float64, confusion TruthConfusion) string {
	return hashString(BuildSummary(states, nil).SummaryDigest,
		formatFloat(agreement), fmt.Sprint(confusion))
}

func BuildHeatmap(img *Image, chunks []Chunk, states []BlockState) ([]byte, []byte, string, error) {
	byIndex := make(map[int]BlockState, len(states))
	keys := make([]string, len(chunks))
	for _, state := range states {
		byIndex[state.BlockIndex] = state
		if state.BlockIndex >= 0 && state.BlockIndex < len(keys) {
			keys[state.BlockIndex] = state.SummaryKey
		}
	}
	rgba := image.NewNRGBA(image.Rect(0, 0, img.Width, img.Height))
	for _, chunk := range chunks {
		state, ok := byIndex[chunk.Index]
		if !ok {
			return nil, nil, "", fmt.Errorf("missing state for block %d", chunk.Index)
		}
		c := heatColor(state)
		for y := chunk.Y0; y < chunk.Y1; y++ {
			for x := chunk.X0; x < chunk.X1; x++ {
				rgba.SetNRGBA(x, y, c)
			}
		}
	}
	var buf []byte
	{
		var b bytesBuffer
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&b, rgba); err != nil {
			return nil, nil, "", DecodeError("encode heatmap", err)
		}
		buf = b.Bytes()
	}
	key := HeatmapKey(keys)
	return rgba.Pix, buf, key, nil
}

func heatColor(state BlockState) color.NRGBA {
	score := clampFloat(state.Stats.TamperScore, 0, 1)
	if state.Tampered {
		return color.NRGBA{
			R: uint8(210 + int(45*score)),
			G: uint8(30 + int(80*(1-score))),
			B: uint8(24 + int(30*(1-score))),
			A: uint8(130 + int(100*score)),
		}
	}
	return color.NRGBA{
		R: uint8(20 + int(80*score)),
		G: uint8(100 + int(90*score)),
		B: uint8(190 + int(55*score)),
		A: uint8(60 + int(80*score)),
	}
}

// bytesBuffer is a small allocation-free alias that keeps the image package
// import local to this file.
type bytesBuffer struct {
	data []byte
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *bytesBuffer) Bytes() []byte { return b.data }
