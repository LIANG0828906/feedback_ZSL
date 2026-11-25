package runner

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"channelpipe"
	"channelpipe/internal/checkpoint"
)

type FailureHook func(imageID string, chunk channelpipe.Chunk, stageIndex, attempt int) error

type Config struct {
	Workers               int
	TileWidth             int
	TileHeight            int
	PerThreadBufferPixels int
	InterruptAfterCommits int
	FailureHook           FailureHook
}

type FailedChunk struct {
	ChunkIndex int    `json:"chunk_index"`
	StageIndex int    `json:"stage_index"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Attempt    int    `json:"attempt"`
}

type ImageResult struct {
	ImageID   string                      `json:"image_id"`
	Stages    []*channelpipe.Buffer       `json:"-"`
	Final     *channelpipe.Buffer         `json:"-"`
	Summaries []channelpipe.BufferSummary `json:"summaries"`
}

type Outcome struct {
	ImageID         string        `json:"image_id"`
	WorkIndexes     []int         `json:"work_indexes"`
	CommittedNow    []int         `json:"committed_now"`
	CommitSequence  []int         `json:"commit_sequence"`
	Failed          []FailedChunk `json:"failed,omitempty"`
	Interrupted     bool          `json:"interrupted"`
	RecoveredChunks int           `json:"recovered_chunks"`
	BufferWaits     []BufferWait  `json:"buffer_waits,omitempty"`
	Result          *ImageResult  `json:"result,omitempty"`
}

type Runner struct {
	store    *checkpoint.Store
	cfg      Config
	gate     *BufferGate
	mu       sync.Mutex
	attempts map[string]int
}

func New(store *checkpoint.Store, cfg Config) *Runner {
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.TileWidth <= 0 {
		cfg.TileWidth = 128
	}
	if cfg.TileHeight <= 0 {
		cfg.TileHeight = 128
	}
	if cfg.PerThreadBufferPixels <= 0 {
		cfg.PerThreadBufferPixels = 1 << 22
	}
	return &Runner{
		store:    store,
		cfg:      cfg,
		gate:     NewBufferGate(cfg.Workers, cfg.PerThreadBufferPixels),
		attempts: map[string]int{},
	}
}

func (r *Runner) Config() Config { return r.cfg }

func (r *Runner) GateWaits() []BufferWait { return r.gate.SnapshotWaits() }

func (r *Runner) RunPlan(ctx context.Context, imageID string, source channelpipe.Buffer, plan channelpipe.Plan) (*Outcome, error) {
	if err := channelpipe.ValidatePlan(&plan); err != nil {
		return nil, err
	}
	chunks := channelpipe.BuildChunks(plan.Width, plan.Height, r.cfg.TileWidth, r.cfg.TileHeight)
	for _, chunk := range chunks {
		if err := r.store.EnsureChunk(imageID, chunk, len(plan.Stages)); err != nil {
			return nil, err
		}
	}
	recovered, err := r.store.RecoverStale(imageID)
	if err != nil {
		return nil, err
	}
	records, err := r.store.CommittedChunkMap(imageID)
	if err != nil {
		return nil, err
	}
	result := allocateResult(imageID, plan)
	work := make([]int, 0, len(chunks))
	for _, chunk := range chunks {
		record, ok := records[chunk.Index]
		if ok && record.State == channelpipe.StateCommitted {
			if err := r.loadCommittedIntoResult(imageID, chunk, result); err != nil {
				return nil, err
			}
			continue
		}
		work = append(work, chunk.Index)
	}

	outcome := &Outcome{ImageID: imageID, WorkIndexes: work, RecoveredChunks: recovered, Result: result}
	if len(work) > 0 {
		task := func(runCtx context.Context, chunk channelpipe.Chunk, attempt int) ([]checkpoint.StageOutput, error) {
			return r.processChunk(runCtx, imageID, source, plan, chunk, attempt)
		}
		if err := r.execute(ctx, imageID, chunks, work, plan, result, outcome, task); err != nil {
			return outcome, err
		}
	}
	sequence, err := r.store.CommitSequence(imageID)
	if err != nil {
		return outcome, err
	}
	outcome.CommitSequence = sequence
	outcome.BufferWaits = r.gate.SnapshotWaits()
	if !outcome.Interrupted {
		summaries, err := summarizeResult(result)
		if err != nil {
			return outcome, err
		}
		result.Summaries = summaries
	}
	return outcome, nil
}

func (r *Runner) RunIncremental(ctx context.Context, imageID string, source channelpipe.Buffer, oldPlan, newPlan channelpipe.Plan, invalidation *channelpipe.InvalidationReport) (*Outcome, *channelpipe.InvalidationReport, error) {
	if invalidation == nil {
		report, err := channelpipe.InvalidatePlanEdit(oldPlan, newPlan, channelpipe.BuildChunks(newPlan.Width, newPlan.Height, r.cfg.TileWidth, r.cfg.TileHeight))
		if err != nil {
			return nil, nil, err
		}
		invalidation = report
	}
	if err := channelpipe.ValidatePlan(&newPlan); err != nil {
		return nil, invalidation, err
	}
	chunks := channelpipe.BuildChunks(newPlan.Width, newPlan.Height, r.cfg.TileWidth, r.cfg.TileHeight)
	for _, chunk := range chunks {
		if err := r.store.EnsureChunk(imageID, chunk, len(newPlan.Stages)); err != nil {
			return nil, invalidation, err
		}
	}
	if _, err := r.store.RecoverStale(imageID); err != nil {
		return nil, invalidation, err
	}

	minStageByChunk := map[int]int{}
	for _, item := range invalidation.Items {
		if item.PlanID != newPlan.ID {
			continue
		}
		current, ok := minStageByChunk[item.ChunkIndex]
		if !ok || item.StageIndex < current {
			minStageByChunk[item.ChunkIndex] = item.StageIndex
		}
	}
	work := make([]int, 0, len(minStageByChunk))
	for chunkIndex := range minStageByChunk {
		work = append(work, chunkIndex)
		if err := r.store.TransitionChunk(imageID, chunkIndex, channelpipe.StatePending); err != nil {
			if !channelpipe.IsCode(err, channelpipe.CodeState) {
				return nil, invalidation, err
			}
		}
	}
	sort.Ints(work)

	result := allocateResult(imageID, newPlan)
	for _, chunk := range chunks {
		if err := r.loadCommittedIntoResult(imageID, chunk, result); err != nil {
			return nil, invalidation, err
		}
	}
	outcome := &Outcome{ImageID: imageID, WorkIndexes: work, Result: result}
	task := func(runCtx context.Context, chunk channelpipe.Chunk, attempt int) ([]checkpoint.StageOutput, error) {
		minStage := minStageByChunk[chunk.Index]
		return r.processIncrementalChunk(runCtx, imageID, source, newPlan, chunk, minStage, attempt)
	}
	if err := r.execute(ctx, imageID, chunks, work, newPlan, result, outcome, task); err != nil {
		return outcome, invalidation, err
	}
	sequence, err := r.store.CommitSequenceForFingerprint(imageID, r.store.Identity().PlanFingerprint)
	if err != nil {
		return outcome, invalidation, err
	}
	outcome.CommitSequence = sequence
	outcome.BufferWaits = r.gate.SnapshotWaits()
	summaries, err := summarizeResult(result)
	if err != nil {
		return outcome, invalidation, err
	}
	result.Summaries = summaries
	return outcome, invalidation, nil
}

type chunkTask func(ctx context.Context, chunk channelpipe.Chunk, attempt int) ([]checkpoint.StageOutput, error)

type job struct {
	index      int
	enqueuedAt time.Time
}

type chunkResult struct {
	index   int
	outputs []checkpoint.StageOutput
	err     error
	stage   int
}

type stageProcessError struct {
	stage int
	err   error
}

func (e *stageProcessError) Error() string { return e.err.Error() }
func (e *stageProcessError) Unwrap() error { return e.err }

func (r *Runner) execute(parent context.Context, imageID string, allChunks []channelpipe.Chunk, work []int, plan channelpipe.Plan, result *ImageResult, outcome *Outcome, task chunkTask) error {
	runCtx, cancel := context.WithCancel(parent)
	defer cancel()
	workers := r.cfg.Workers
	if workers > len(work) {
		workers = len(work)
	}
	if workers <= 0 {
		return nil
	}
	jobs := make(chan job)
	results := make(chan chunkResult, len(work))
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for current := range jobs {
				queueWait := time.Since(current.enqueuedAt).Milliseconds()
				_ = r.store.RecordQueueEvent(checkpoint.QueueEvent{ImageID: imageID, ChunkIndex: current.index, Reason: "worker-slot", WaitMS: queueWait})
				attempt := r.nextAttempt(imageID, current.index)
				if err := r.store.StartChunk(imageID, current.index); err != nil {
					results <- chunkResult{index: current.index, err: err}
					continue
				}
				chunk := allChunks[current.index]
				cost := estimateCost(chunk.Rect, plan)
				bufferWait, err := r.gate.Acquire(runCtx, cost, "pixel-buffer-budget")
				_ = r.store.RecordQueueEvent(checkpoint.QueueEvent{ImageID: imageID, ChunkIndex: current.index, Reason: "pixel-buffer-budget", WaitMS: bufferWait})
				if err != nil {
					results <- chunkResult{index: current.index, err: err}
					continue
				}
				outputs, err := task(runCtx, chunk, attempt)
				r.gate.Release(cost)
				if err != nil {
					if runCtx.Err() == nil {
						_ = r.store.FailChunk(imageID, current.index, err)
					}
					results <- chunkResult{index: current.index, outputs: outputs, err: err}
					continue
				}
				results <- chunkResult{index: current.index, outputs: outputs}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, index := range work {
			select {
			case jobs <- job{index: index, enqueuedAt: time.Now()}:
			case <-runCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	byIndex := map[int]chunkResult{}
	position := 0
	commits := 0
	for received := range results {
		byIndex[received.index] = received
		for position < len(work) {
			index := work[position]
			ready, ok := byIndex[index]
			if !ok {
				break
			}
			position++
			delete(byIndex, index)
			if ready.err != nil {
				outcome.Failed = append(outcome.Failed, toFailedChunk(ready, r.attemptCount(imageID, index)))
				continue
			}
			if outcome.Interrupted {
				continue
			}
			if r.cfg.InterruptAfterCommits > 0 && commits >= r.cfg.InterruptAfterCommits {
				outcome.Interrupted = true
				cancel()
				continue
			}
			chunk := allChunks[index]
			commit := checkpoint.CommitChunk{
				ImageID:    imageID,
				Chunk:      chunk,
				ChunkOrder: index,
				Outputs:    ready.outputs,
			}
			if _, err := r.store.CommitChunk(runCtx, commit); err != nil {
				outcome.Failed = append(outcome.Failed, FailedChunk{ChunkIndex: index, Code: errorCode(err), Message: err.Error(), Attempt: r.attemptCount(imageID, index)})
				continue
			}
			for stageIndex, output := range ready.outputs {
				if output.Buffer == nil {
					continue
				}
				copyChunkToFull(result.Stages[stageIndex], output.Buffer, chunk.Rect)
			}
			outcome.CommittedNow = append(outcome.CommittedNow, index)
			commits++
			if r.cfg.InterruptAfterCommits > 0 && commits >= r.cfg.InterruptAfterCommits {
				outcome.Interrupted = true
				cancel()
			}
		}
	}
	wg.Wait()
	if outcome.Interrupted {
		return channelpipe.NewError(channelpipe.CodeState, "run interrupted after %d committed chunks", commits)
	}
	return nil
}

func (r *Runner) processChunk(ctx context.Context, imageID string, source channelpipe.Buffer, plan channelpipe.Plan, chunk channelpipe.Chunk, attempt int) (outputs []checkpoint.StageOutput, err error) {
	currentStage := -1
	defer func() {
		if err != nil && currentStage >= 0 {
			err = &stageProcessError{stage: currentStage, err: err}
		}
	}()
	sourceChunk := extractChunk(source, chunk.Rect)
	regionHash, err := channelpipe.RegionHash(&sourceChunk)
	if err != nil {
		return nil, err
	}
	upstreamKey := "source:" + regionHash
	input := sourceChunk
	outputs = make([]checkpoint.StageOutput, 0, len(plan.Stages))
	for stageIndex, stage := range plan.Stages {
		currentStage = stageIndex
		if err := ctx.Err(); err != nil {
			return outputs, err
		}
		if r.cfg.FailureHook != nil {
			if hookErr := r.cfg.FailureHook(imageID, chunk, stageIndex, attempt); hookErr != nil {
				_ = r.store.FailStage(imageID, chunk.Index, stageIndex, hookErr)
				return outputs, hookErr
			}
		}
		inputHash, err := channelpipe.RegionHash(&input)
		if err != nil {
			return outputs, err
		}
		cacheKey, err := channelpipe.StageCacheKey(inputHash, stageIndex, stage, upstreamKey)
		if err != nil {
			return outputs, err
		}
		stageResult, err := channelpipe.ApplyStage(stage, input)
		if err != nil {
			_ = r.store.FailStage(imageID, chunk.Index, stageIndex, err)
			return outputs, err
		}
		outputHash, err := channelpipe.RegionHash(&stageResult.Buffer)
		if err != nil {
			return outputs, err
		}
		outputs = append(outputs, checkpoint.StageOutput{
			StageIndex: stageIndex,
			StageID:    stage.ID,
			Buffer:     &stageResult.Buffer,
			CacheKey:   cacheKey,
			OutputHash: outputHash,
			Dropped:    stageResult.Dropped,
		})
		input = stageResult.Buffer
		upstreamKey = cacheKey
	}
	return outputs, nil
}

func (r *Runner) processIncrementalChunk(ctx context.Context, imageID string, source channelpipe.Buffer, plan channelpipe.Plan, chunk channelpipe.Chunk, minStage, attempt int) (outputs []checkpoint.StageOutput, err error) {
	currentStage := minStage
	defer func() {
		if err != nil && currentStage >= 0 {
			err = &stageProcessError{stage: currentStage, err: err}
		}
	}()
	outputs = make([]checkpoint.StageOutput, len(plan.Stages))
	var input channelpipe.Buffer
	var upstreamKey string

	if minStage > 0 {
		persisted, err := r.store.LoadChunkOutputs(imageID, chunk.Index)
		if err != nil {
			return nil, err
		}
		byIndex := map[int]checkpoint.PersistedOutput{}
		for _, output := range persisted {
			byIndex[output.StageIndex] = output
		}
		for stageIndex := 0; stageIndex < minStage; stageIndex++ {
			old, ok := byIndex[stageIndex]
			if !ok {
				return nil, channelpipe.FieldError(channelpipe.CodeCheckpoint, "chunk_outputs", "missing reusable stage %d for incremental chunk %d", stageIndex, chunk.Index)
			}
			outputs[stageIndex] = checkpoint.StageOutput{
				StageIndex: stageIndex,
				StageID:    plan.Stages[stageIndex].ID,
				Buffer:     old.Buffer,
				CacheKey:   old.CacheKey,
				OutputHash: old.OutputHash,
				Dropped:    old.Dropped,
			}
		}
		input = *byIndex[minStage-1].Buffer
		upstreamKey = byIndex[minStage-1].CacheKey
	} else {
		input = extractChunk(source, chunk.Rect)
		regionHash, err := channelpipe.RegionHash(&input)
		if err != nil {
			return nil, err
		}
		upstreamKey = "source:" + regionHash
	}

	for stageIndex := minStage; stageIndex < len(plan.Stages); stageIndex++ {
		currentStage = stageIndex
		if err := ctx.Err(); err != nil {
			return outputs, err
		}
		if r.cfg.FailureHook != nil {
			if hookErr := r.cfg.FailureHook(imageID, chunk, stageIndex, attempt); hookErr != nil {
				_ = r.store.FailStage(imageID, chunk.Index, stageIndex, hookErr)
				return outputs, hookErr
			}
		}
		inputHash, err := channelpipe.RegionHash(&input)
		if err != nil {
			return outputs, err
		}
		stage := plan.Stages[stageIndex]
		cacheKey, err := channelpipe.StageCacheKey(inputHash, stageIndex, stage, upstreamKey)
		if err != nil {
			return outputs, err
		}
		stageResult, err := channelpipe.ApplyStage(stage, input)
		if err != nil {
			_ = r.store.FailStage(imageID, chunk.Index, stageIndex, err)
			return outputs, err
		}
		outputHash, err := channelpipe.RegionHash(&stageResult.Buffer)
		if err != nil {
			return outputs, err
		}
		outputs[stageIndex] = checkpoint.StageOutput{
			StageIndex: stageIndex,
			StageID:    stage.ID,
			Buffer:     &stageResult.Buffer,
			CacheKey:   cacheKey,
			OutputHash: outputHash,
			Dropped:    stageResult.Dropped,
		}
		input = stageResult.Buffer
		upstreamKey = cacheKey
	}
	return outputs, nil
}

func allocateResult(imageID string, plan channelpipe.Plan) *ImageResult {
	result := &ImageResult{ImageID: imageID, Stages: make([]*channelpipe.Buffer, len(plan.Stages))}
	for i, stage := range plan.Stages {
		result.Stages[i] = &channelpipe.Buffer{
			Width:    plan.Width,
			Height:   plan.Height,
			Depth:    stage.TargetDepth,
			Channels: append([]channelpipe.Channel(nil), stage.TargetChannels...),
			Pixels:   make([]uint16, plan.Width*plan.Height*len(stage.TargetChannels)),
		}
	}
	if len(result.Stages) > 0 {
		result.Final = result.Stages[len(result.Stages)-1]
	}
	return result
}

func (r *Runner) loadCommittedIntoResult(imageID string, chunk channelpipe.Chunk, result *ImageResult) error {
	outputs, err := r.store.LoadChunkOutputs(imageID, chunk.Index)
	if err != nil {
		return err
	}
	for _, output := range outputs {
		if output.StageIndex >= len(result.Stages) || output.Buffer == nil {
			continue
		}
		copyChunkToFull(result.Stages[output.StageIndex], output.Buffer, chunk.Rect)
	}
	return nil
}

func summarizeResult(result *ImageResult) ([]channelpipe.BufferSummary, error) {
	summaries := make([]channelpipe.BufferSummary, len(result.Stages))
	for i, buffer := range result.Stages {
		summary, err := channelpipe.SummarizeBuffer(i, buffer)
		if err != nil {
			return nil, err
		}
		summaries[i] = summary
	}
	return summaries, nil
}

func extractChunk(source channelpipe.Buffer, rect channelpipe.Rect) channelpipe.Buffer {
	channels := source.ChannelCount()
	chunk := channelpipe.Buffer{
		Width:    rect.Width,
		Height:   rect.Height,
		Depth:    source.Depth,
		Channels: append([]channelpipe.Channel(nil), source.Channels...),
		Pixels:   make([]uint16, rect.Width*rect.Height*channels),
	}
	for y := 0; y < rect.Height; y++ {
		sourceY := rect.Y + y
		for x := 0; x < rect.Width; x++ {
			sourceX := rect.X + x
			sourceBase := (sourceY*source.Width + sourceX) * channels
			targetBase := (y*rect.Width + x) * channels
			copy(chunk.Pixels[targetBase:targetBase+channels], source.Pixels[sourceBase:sourceBase+channels])
		}
	}
	return chunk
}

func copyChunkToFull(full, chunk *channelpipe.Buffer, rect channelpipe.Rect) {
	channels := full.ChannelCount()
	if chunk.ChannelCount() != channels {
		return
	}
	for y := 0; y < rect.Height; y++ {
		for x := 0; x < rect.Width; x++ {
			fullX := rect.X + x
			fullY := rect.Y + y
			fullBase := (fullY*full.Width + fullX) * channels
			chunkBase := (y*chunk.Width + x) * channels
			copy(full.Pixels[fullBase:fullBase+channels], chunk.Pixels[chunkBase:chunkBase+channels])
		}
	}
}

func estimateCost(rect channelpipe.Rect, plan channelpipe.Plan) int {
	channels := 4
	for _, stage := range plan.Stages {
		channels += len(stage.TargetChannels)
	}
	return rect.PixelCount() * channels
}

func (r *Runner) nextAttempt(imageID string, chunkIndex int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := imageID + "#" + itoa(chunkIndex)
	r.attempts[key]++
	return r.attempts[key]
}

func (r *Runner) attemptCount(imageID string, chunkIndex int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts[imageID+"#"+itoa(chunkIndex)]
}

func toFailedChunk(result chunkResult, attempt int) FailedChunk {
	code := "E_INTERNAL"
	if domainErr, ok := channelpipe.AsError(result.err); ok {
		code = domainErr.Code
	}
	stageIndex := result.stage
	var stageErr *stageProcessError
	if errors.As(result.err, &stageErr) {
		stageIndex = stageErr.stage
	}
	return FailedChunk{ChunkIndex: result.index, StageIndex: stageIndex, Code: code, Message: result.err.Error(), Attempt: attempt}
}

func errorCode(err error) string {
	if domainErr, ok := channelpipe.AsError(err); ok {
		return domainErr.Code
	}
	return "E_INTERNAL"
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}
