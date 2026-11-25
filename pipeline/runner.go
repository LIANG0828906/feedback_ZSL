package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"texturekernel"
	"texturekernel/store"
)

type Mode string

const (
	ModeFull        Mode = "full"
	ModeIncremental Mode = "incremental"
	ModeResume      Mode = "resume"
)

type Config struct {
	ChunkWidth           int
	ChunkHeight          int
	Workers              int
	PixelBufferPerWorker int64
	ParamVersion         string
	AlgorithmVersion     string
	QueueWaitThreshold   time.Duration
	OnCommit             func(CommitRef)
}

func (c Config) withDefaults() (Config, error) {
	if c.ChunkWidth == 0 {
		c.ChunkWidth = 128
	}
	if c.ChunkHeight == 0 {
		c.ChunkHeight = 128
	}
	if c.Workers == 0 {
		c.Workers = 4
	}
	if c.PixelBufferPerWorker == 0 {
		c.PixelBufferPerWorker = 16 << 20
	}
	if c.ParamVersion == "" {
		c.ParamVersion = texture.DefaultParamVersion
	}
	if c.AlgorithmVersion == "" {
		c.AlgorithmVersion = texture.AlgorithmVersion
	}
	if c.QueueWaitThreshold < 0 {
		return c, errf(texture.CodePara, "$.config.queue_wait_threshold", "queue_wait_threshold", "must be >= 0")
	}
	if c.ChunkWidth <= 0 {
		return c, errf(texture.CodePara, "$.config.chunk_width", "chunk_width", "must be > 0")
	}
	if c.ChunkHeight <= 0 {
		return c, errf(texture.CodePara, "$.config.chunk_height", "chunk_height", "must be > 0")
	}
	if c.Workers <= 0 {
		return c, errf(texture.CodePara, "$.config.workers", "workers", "must be > 0")
	}
	if c.Workers > 64 {
		return c, errf(texture.CodePara, "$.config.workers", "workers", "must be <= 64")
	}
	if c.PixelBufferPerWorker <= 0 {
		return c, errf(texture.CodePara, "$.config.pixel_buffer_per_worker", "pixel_buffer_per_worker", "must be > 0")
	}
	return c, nil
}

type Options struct {
	Mode          Mode
	RunID         string
	PreviousRunID string
	PreviousPlan  *texture.Plan
}

type CommitRef struct {
	ChunkX    int   `json:"chunk_x"`
	ChunkY    int   `json:"chunk_y"`
	CommitSeq int64 `json:"commit_seq"`
}

type StageDigest struct {
	EntryIndex int    `json:"entry_index"`
	Digest     string `json:"digest"`
}

type QueueEvent struct {
	ChunkX     int    `json:"chunk_x"`
	ChunkY     int    `json:"chunk_y"`
	Reason     string `json:"reason"`
	WaitMillis int64  `json:"wait_ms"`
	WaitMicros int64  `json:"wait_us"`
}

type Result struct {
	Mode             Mode                  `json:"mode"`
	RunID            string                `json:"run_id"`
	Final            *texture.Image        `json:"-"`
	FinalHash        string                `json:"final_hash"`
	StageHashes      []StageDigest         `json:"stage_hashes"`
	CommitSequence   []CommitRef           `json:"commit_sequence"`
	Progress         texture.StateProgress `json:"progress"`
	ReusedChunks     int                   `json:"reused_chunks"`
	ComputedChunks   int                   `json:"computed_chunks"`
	CheckpointWiped  bool                  `json:"checkpoint_wiped"`
	QueueEvents      []QueueEvent          `json:"queue_events"`
	MaxTaskBuffer    int64                 `json:"max_task_buffer_bytes"`
	PixelBufferLimit int64                 `json:"pixel_buffer_per_worker"`
}

type Runner struct {
	store  *store.Store
	config Config
}

func NewRunner(s *store.Store, cfg Config) (*Runner, error) {
	if s == nil {
		return nil, errf(texture.CodePara, "$.store", "store", "store is nil")
	}
	normalized, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Runner{store: s, config: normalized}, nil
}

func (r *Runner) Run(ctx context.Context, source *texture.Image, plan *texture.Plan, opts Options) (*Result, error) {
	if err := source.Validate(); err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if source.Width != plan.Source.Width || source.Height != plan.Source.Height ||
		source.BitDepth != plan.Source.BitDepth ||
		texture.LayoutString(source.Layout) != texture.LayoutString(plan.Source.Layout) {
		return nil, errf(texture.CodePara, "$.source", "source", "source image does not match plan metadata")
	}
	if opts.RunID == "" {
		return nil, errf(texture.CodePara, "$.options.run_id", "run_id", "must not be empty")
	}
	switch opts.Mode {
	case ModeFull, ModeIncremental, ModeResume:
	default:
		return nil, errf(texture.CodePara, "$.options.mode", "mode", "unsupported mode %q", opts.Mode)
	}
	if opts.Mode == ModeIncremental {
		if opts.PreviousRunID == "" {
			return nil, errf(texture.CodePara, "$.options.previous_run_id", "previous_run_id", "incremental mode requires a previous run")
		}
		if opts.PreviousRunID == opts.RunID {
			return nil, errf(texture.CodePara, "$.options.previous_run_id", "previous_run_id", "must differ from the new run id")
		}
		if opts.PreviousPlan == nil {
			return nil, errf(texture.CodePara, "$.options.previous_plan", "previous_plan", "incremental mode requires the previous plan")
		}
	}

	chunks, err := texture.BuildChunks(source.Width, source.Height, r.config.ChunkWidth, r.config.ChunkHeight)
	if err != nil {
		return nil, err
	}
	tileBytes := int64(r.config.ChunkWidth * r.config.ChunkHeight * 4 * 2)
	maxTileBytes := tileBytes * int64(len(plan.Entries)+2)
	if maxTileBytes > r.config.PixelBufferPerWorker {
		return nil, errf(texture.CodePara, "$.config.pixel_buffer_per_worker", "pixel_buffer_per_worker",
			"limit %d is below required tile working set %d", r.config.PixelBufferPerWorker, maxTileBytes)
	}

	meta := r.metaFor(source, plan, opts.RunID)
	result := &Result{
		Mode: opts.Mode, RunID: opts.RunID,
		PixelBufferLimit: r.config.PixelBufferPerWorker,
		MaxTaskBuffer:    maxTileBytes,
	}

	if opts.Mode == ModeFull || opts.Mode == ModeIncremental {
		if err := r.store.WipeRun(ctx, opts.RunID); err != nil {
			return nil, err
		}
	}
	prepared, err := r.store.PrepareRun(ctx, meta)
	if err != nil {
		return nil, err
	}
	if opts.Mode == ModeResume && prepared.Wiped {
		result.CheckpointWiped = true
	}

	var loaded []store.ChunkCommit
	if opts.Mode == ModeResume {
		loaded, err = r.store.LoadChunks(ctx, meta)
		if err == nil {
			err = r.validateLoaded(source, plan, chunks, loaded)
		}
		if err != nil {
			// A single hash/version/metadata mismatch invalidates the whole
			// snapshot. There is no partial salvage.
			if wipeErr := r.store.WipeRun(ctx, opts.RunID); wipeErr != nil {
				return nil, wipeErr
			}
			if _, prepareErr := r.store.PrepareRun(ctx, meta); prepareErr != nil {
				return nil, prepareErr
			}
			loaded = nil
			result.CheckpointWiped = true
		}
	}

	cacheIndex := make(map[string]store.StageOutput)
	reuseChunks := make(map[int]store.ChunkCommit)
	if opts.Mode == ModeIncremental {
		previous, ok := r.previousState(ctx, source, plan, opts)
		if ok {
			cacheIndex = previous.cache
			invalidations, invalidateErr := texture.Invalidate(opts.PreviousPlan, plan, r.config.ChunkWidth, r.config.ChunkHeight)
			if invalidateErr != nil {
				return nil, invalidateErr
			}
			invalidChunks := make(map[string]bool)
			for _, item := range invalidations {
				invalidChunks[fmt.Sprintf("%d/%d", item.ChunkX, item.ChunkY)] = true
			}
			for i, chunk := range chunks {
				if invalidChunks[fmt.Sprintf("%d/%d", chunk.X, chunk.Y)] {
					continue
				}
				if i < len(previous.chunks) {
					reuseChunks[i] = previous.chunks[i]
				}
			}
		}
	}

	machine, err := texture.NewStateMachine(len(plan.Entries), len(chunks))
	if err != nil {
		return nil, err
	}
	digests := make([]*digestBuilder, len(plan.Entries))
	for i := range digests {
		digests[i] = newDigestBuilder()
	}
	result.CommitSequence = make([]CommitRef, 0, len(chunks))
	for i := range loaded {
		commit := loaded[i]
		ref := CommitRef{ChunkX: commit.ChunkX, ChunkY: commit.ChunkY, CommitSeq: commit.CommitSeq}
		result.CommitSequence = append(result.CommitSequence, ref)
		for _, stage := range commit.Stages {
			digests[stage.EntryIndex].Add(ref, stage)
			if err := machine.Begin(stage.EntryIndex, i); err != nil {
				return nil, err
			}
			if err := machine.Commit(stage.EntryIndex, i, snapshotFor(stage, plan, commit, i)); err != nil {
				return nil, err
			}
		}
		result.ReusedChunks++
	}

	startIndex := len(loaded)
	if startIndex < len(chunks) {
		computed, reused, events, err := r.runChunks(ctx, source, plan, meta, chunks, startIndex, machine, cacheIndex, reuseChunks, &result.CommitSequence, digests)
		if err != nil {
			result.ComputedChunks = computed
			result.ReusedChunks += reused
			result.QueueEvents = events
			result.Progress = machine.Progress()
			result.FinalHash = ""
			result.StageHashes = digestValues(digests)
			if errors.Is(err, texture.ErrCanceled) || errors.Is(err, context.Canceled) {
				return result, texture.ErrCanceled
			}
			return result, err
		}
		result.ComputedChunks = computed
		result.ReusedChunks += reused
		result.QueueEvents = events
	}

	if len(result.CommitSequence) != len(chunks) {
		return result, errf(texture.CodeStore, "$.checkpoint", "commit_sequence", "expected %d commits, got %d", len(chunks), len(result.CommitSequence))
	}
	final, err := r.composeFinal(ctx, meta, plan, chunks)
	if err != nil {
		return result, err
	}
	result.Final = final
	result.FinalHash = final.Hash()
	result.StageHashes = digestValues(digests)
	result.Progress = machine.Progress()
	return result, nil
}

func (r *Runner) metaFor(source *texture.Image, plan *texture.Plan, runID string) store.RunMeta {
	fingerprint := plan.Fingerprint
	if fingerprint == "" {
		fingerprint = plan.FingerprintValue()
	}
	return store.RunMeta{
		RunID: runID, SourceHash: source.Hash(), PlanFingerprint: fingerprint,
		ParamVersion: r.config.ParamVersion, AlgorithmVersion: r.config.AlgorithmVersion,
		Width: source.Width, Height: source.Height, BitDepth: source.BitDepth,
		Layout:     texture.LayoutString(source.Layout),
		ChunkWidth: r.config.ChunkWidth, ChunkHeight: r.config.ChunkHeight,
	}
}

type priorState struct {
	cache  map[string]store.StageOutput
	chunks []store.ChunkCommit
}

func (r *Runner) previousState(ctx context.Context, source *texture.Image, plan *texture.Plan, opts Options) (priorState, bool) {
	if opts.PreviousPlan == nil {
		return priorState{}, false
	}
	prevPlan := opts.PreviousPlan
	if err := prevPlan.Validate(); err != nil {
		return priorState{}, false
	}
	if source.Width != prevPlan.Source.Width || source.Height != prevPlan.Source.Height ||
		source.BitDepth != prevPlan.Source.BitDepth ||
		texture.LayoutString(source.Layout) != texture.LayoutString(prevPlan.Source.Layout) {
		return priorState{}, false
	}
	prevMeta := r.metaFor(source, prevPlan, opts.PreviousRunID)
	stored, found, err := r.store.ReadRunMeta(ctx, opts.PreviousRunID)
	if err != nil || !found || !sameMeta(stored, prevMeta) {
		return priorState{}, false
	}
	chunks, err := texture.BuildChunks(source.Width, source.Height, r.config.ChunkWidth, r.config.ChunkHeight)
	if err != nil {
		return priorState{}, false
	}
	loaded, err := r.store.LoadChunks(ctx, prevMeta)
	if err != nil || r.validateLoaded(source, prevPlan, chunks, loaded) != nil || len(loaded) != len(chunks) {
		return priorState{}, false
	}
	index, err := r.store.LoadCacheIndex(ctx, opts.PreviousRunID)
	if err != nil {
		return priorState{}, false
	}
	return priorState{cache: index, chunks: loaded}, true
}

func sameMeta(a, b store.RunMeta) bool {
	return a.RunID == b.RunID && a.SourceHash == b.SourceHash &&
		a.PlanFingerprint == b.PlanFingerprint && a.ParamVersion == b.ParamVersion &&
		a.AlgorithmVersion == b.AlgorithmVersion && a.Width == b.Width &&
		a.Height == b.Height && a.BitDepth == b.BitDepth && a.Layout == b.Layout &&
		a.ChunkWidth == b.ChunkWidth && a.ChunkHeight == b.ChunkHeight
}

type taskResult struct {
	index  int
	commit store.ChunkCommit
	event  *QueueEvent
	reused bool
	err    error
}

func (r *Runner) runChunks(
	ctx context.Context,
	source *texture.Image,
	plan *texture.Plan,
	meta store.RunMeta,
	chunks []texture.Chunk,
	start int,
	machine *texture.StateMachine,
	cacheIndex map[string]store.StageOutput,
	reuseChunks map[int]store.ChunkCommit,
	commitSeq *[]CommitRef,
	digests []*digestBuilder,
) (int, int, []QueueEvent, error) {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan job, r.config.Workers)
	resultSlots := make([]chan taskResult, len(chunks))
	for i := start; i < len(chunks); i++ {
		resultSlots[i] = make(chan taskResult, 1)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < r.config.Workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				wait := time.Since(j.enqueued)
				var event *QueueEvent
				if wait >= r.config.QueueWaitThreshold && wait > 0 {
					micros := wait.Microseconds()
					if micros == 0 {
						micros = 1
					}
					c := QueueEvent{
						ChunkX: chunks[j.index].X, ChunkY: chunks[j.index].Y,
						Reason:     fmt.Sprintf("pixel-buffer/worker-slot busy (limit %d bytes)", r.config.PixelBufferPerWorker),
						WaitMillis: wait.Milliseconds(),
						WaitMicros: micros,
					}
					event = &c
				}
				beginErr := error(nil)
				for e := range plan.Entries {
					if err := machine.Begin(e, j.index); err != nil {
						beginErr = err
						break
					}
				}
				if beginErr != nil {
					resultSlots[j.index] <- taskResult{index: j.index, err: beginErr, event: event}
					continue
				}
				if reused, ok := reuseChunks[j.index]; ok {
					commit := reused
					commit.CommitSeq = int64(j.index)
					commit.ChunkX = chunks[j.index].X
					commit.ChunkY = chunks[j.index].Y
					for i := range commit.Stages {
						commit.Stages[i].PlanFingerprint = plan.Fingerprint
						commit.Stages[i].ParamVersion = r.config.ParamVersion
						commit.Stages[i].AlgorithmVersion = r.config.AlgorithmVersion
						commit.Stages[i].ParameterSnapshot = texture.EntryParameterSnapshot(plan.Entries[commit.Stages[i].EntryIndex])
					}
					resultSlots[j.index] <- taskResult{index: j.index, commit: commit, event: event, reused: true}
					continue
				}
				commit, err := computeChunk(source, plan, chunks[j.index], cacheIndex, int64(j.index), r.config.ParamVersion, r.config.AlgorithmVersion)
				if err != nil {
					for e := range plan.Entries {
						_ = machine.Fail(e, j.index, err)
					}
					resultSlots[j.index] <- taskResult{index: j.index, err: err, event: event}
					continue
				}
				resultSlots[j.index] <- taskResult{index: j.index, commit: commit, event: event}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i := start; i < len(chunks); i++ {
			select {
			case jobs <- job{index: i, enqueued: time.Now()}:
			case <-workCtx.Done():
				return
			}
		}
	}()

	var events []QueueEvent
	computed := 0
	reused := 0
	for next := start; next < len(chunks); next++ {
		select {
		case <-ctx.Done():
			cancel()
			wg.Wait()
			return computed, reused, events, texture.ErrCanceled
		case result := <-resultSlots[next]:
			if result.event != nil {
				events = append(events, *result.event)
			}
			if result.err != nil {
				cancel()
				wg.Wait()
				return computed, reused, events, result.err
			}
			if ctx.Err() != nil {
				cancel()
				wg.Wait()
				return computed, reused, events, texture.ErrCanceled
			}
			if err := r.store.CommitChunk(ctx, meta, result.commit); err != nil {
				for _, stage := range result.commit.Stages {
					_ = machine.Fail(stage.EntryIndex, next, err)
				}
				cancel()
				wg.Wait()
				return computed, reused, events, err
			}
			for _, stage := range result.commit.Stages {
				if err := machine.Commit(stage.EntryIndex, next, snapshotForStage(stage, plan, result.commit, next)); err != nil {
					cancel()
					wg.Wait()
					return computed, reused, events, err
				}
			}
			ref := CommitRef{ChunkX: result.commit.ChunkX, ChunkY: result.commit.ChunkY, CommitSeq: result.commit.CommitSeq}
			*commitSeq = append(*commitSeq, ref)
			for _, stage := range result.commit.Stages {
				digests[stage.EntryIndex].Add(ref, stage)
			}
			if result.reused {
				reused++
			} else {
				computed++
			}
			if r.config.OnCommit != nil {
				r.config.OnCommit(ref)
			}
			if ctx.Err() != nil {
				cancel()
				wg.Wait()
				return computed, reused, events, texture.ErrCanceled
			}
		}
	}
	cancel()
	wg.Wait()
	sort.Slice(events, func(i, j int) bool {
		if events[i].ChunkY != events[j].ChunkY {
			return events[i].ChunkY < events[j].ChunkY
		}
		return events[i].ChunkX < events[j].ChunkX
	})
	return computed, reused, events, nil
}

type job struct {
	index    int
	enqueued time.Time
}

func computeChunk(source *texture.Image, plan *texture.Plan, chunk texture.Chunk, cacheIndex map[string]store.StageOutput, seq int64, paramVersion, algorithmVersion string) (store.ChunkCommit, error) {
	current := cropImage(source, chunk.Rect)
	commit := store.ChunkCommit{ChunkX: chunk.X, ChunkY: chunk.Y, CommitSeq: seq}
	upstream := ""
	for _, entry := range plan.Entries {
		localEntry, covered := localizeEntry(entry, chunk.Rect)
		inputHash := inputRegionHash(chunk, current, covered)
		paramHash := texture.EntryParameterHashForTile(entry, chunk.Rect)
		key := texture.BuildCacheKey(inputHash, entry.Index, paramHash, upstream)
		var output *texture.Image
		if cached, ok := cacheIndex[store.CacheLookupKey(entry.Index, key)]; ok && cacheCompatible(cached.Image, current, localEntry, covered) {
			output = cached.Image.Clone()
		} else {
			if covered.Empty() {
				output = current.Clone()
			} else {
				result, err := texture.ApplyEntry(current, plan, localEntry)
				if err != nil {
					return store.ChunkCommit{}, err
				}
				output = result.Image
			}
		}
		if output == nil {
			return store.ChunkCommit{}, errf(texture.CodeStore, "$.cache", "image", "stage produced nil image")
		}
		stage := store.StageOutput{
			EntryIndex: entry.Index, CacheKey: key, InputHash: inputHash,
			OutputHash: output.Hash(), PlanFingerprint: plan.Fingerprint,
			ParamVersion: paramVersion, AlgorithmVersion: algorithmVersion,
			ParameterSnapshot: texture.EntryParameterSnapshot(entry),
			Image:             output,
		}
		commit.Stages = append(commit.Stages, stage)
		upstream = key
		current = output
	}
	return commit, nil
}

func cacheCompatible(cached, current *texture.Image, entry texture.Entry, covered texture.Rect) bool {
	if cached == nil || current == nil {
		return false
	}
	if cached.Width != current.Width || cached.Height != current.Height {
		return false
	}
	if covered.Empty() {
		return cached.BitDepth == current.BitDepth && texture.LayoutString(cached.Layout) == texture.LayoutString(current.Layout)
	}
	formatChanges := current.BitDepth != entry.TargetBitDepth ||
		texture.LayoutString(current.Layout) != texture.LayoutString(entry.TargetChannels)
	if formatChanges {
		return cached.BitDepth == entry.TargetBitDepth &&
			texture.LayoutString(cached.Layout) == texture.LayoutString(entry.TargetChannels)
	}
	return cached.BitDepth == current.BitDepth &&
		texture.LayoutString(cached.Layout) == texture.LayoutString(current.Layout)
}

func cropImage(source *texture.Image, rect texture.Rect) *texture.Image {
	out := &texture.Image{
		Width: rect.Width, Height: rect.Height, BitDepth: source.BitDepth,
		Layout: append([]texture.Channel(nil), source.Layout...),
		Pix:    make([]uint16, rect.Width*rect.Height*len(source.Layout)),
	}
	channels := len(source.Layout)
	for y := 0; y < rect.Height; y++ {
		srcStart := ((rect.Y+y)*source.Width + rect.X) * channels
		copy(out.Pix[y*rect.Width*channels:(y+1)*rect.Width*channels], source.Pix[srcStart:srcStart+rect.Width*channels])
	}
	return out
}

func localizeEntry(entry texture.Entry, tile texture.Rect) (texture.Entry, texture.Rect) {
	local := entry
	intersection := entry.Coverage.Intersection(tile)
	if intersection.Empty() {
		local.Coverage = texture.Rect{}
		return local, texture.Rect{}
	}
	local.Coverage = texture.Rect{
		X: intersection.X - tile.X, Y: intersection.Y - tile.Y,
		Width: intersection.Width, Height: intersection.Height,
	}
	return local, local.Coverage
}

func inputRegionHash(chunk texture.Chunk, image *texture.Image, region texture.Rect) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "%d/%d:%s", chunk.X, chunk.Y, texture.HashRegion(image, region))
	return hex.EncodeToString(sum.Sum(nil))
}

func (r *Runner) validateLoaded(source *texture.Image, plan *texture.Plan, chunks []texture.Chunk, loaded []store.ChunkCommit) error {
	if len(loaded) > len(chunks) {
		return errf(texture.CodeStore, "$.checkpoint", "chunks", "more persisted chunks than the plan")
	}
	for i, commit := range loaded {
		if commit.CommitSeq != int64(i) || commit.ChunkX != chunks[i].X || commit.ChunkY != chunks[i].Y {
			return errf(texture.CodeStore, "$.checkpoint", "commit_sequence", "chunk %d is not in stable row-major sequence", i)
		}
		if len(commit.Stages) != len(plan.Entries) {
			return errf(texture.CodeStore, "$.checkpoint", "stages", "chunk %d has %d stages, want %d", i, len(commit.Stages), len(plan.Entries))
		}
		current := cropImage(source, chunks[i].Rect)
		upstream := ""
		for e, entry := range plan.Entries {
			stage := commit.Stages[e]
			localEntry, covered := localizeEntry(entry, chunks[i].Rect)
			inputHash := inputRegionHash(chunks[i], current, covered)
			paramHash := texture.EntryParameterHashForTile(entry, chunks[i].Rect)
			key := texture.BuildCacheKey(inputHash, entry.Index, paramHash, upstream)
			if stage.EntryIndex != entry.Index || stage.InputHash != inputHash || stage.CacheKey != key {
				return errf(texture.CodeStore, "$.checkpoint", "cache_key", "cache chain mismatch at entry %d chunk (%d,%d)", entry.Index, commit.ChunkX, commit.ChunkY)
			}
			if stage.PlanFingerprint != plan.Fingerprint || stage.ParamVersion != r.config.ParamVersion || stage.AlgorithmVersion != r.config.AlgorithmVersion {
				return errf(texture.CodeStore, "$.checkpoint", "version", "version mismatch at entry %d chunk (%d,%d)", entry.Index, commit.ChunkX, commit.ChunkY)
			}
			if stage.ParameterSnapshot != texture.EntryParameterSnapshot(entry) {
				return errf(texture.CodeStore, "$.checkpoint", "parameter_snapshot", "parameter snapshot mismatch at entry %d chunk (%d,%d)", entry.Index, commit.ChunkX, commit.ChunkY)
			}
			if stage.Image == nil || !cacheCompatible(stage.Image, current, localEntry, covered) {
				return errf(texture.CodeStore, "$.checkpoint", "image", "image metadata mismatch at entry %d chunk (%d,%d)", entry.Index, commit.ChunkX, commit.ChunkY)
			}
			if stage.OutputHash != stage.Image.Hash() {
				return errf(texture.CodeStore, "$.checkpoint", "content_hash", "output hash mismatch at entry %d chunk (%d,%d)", entry.Index, commit.ChunkX, commit.ChunkY)
			}
			current = stage.Image
			upstream = key
		}
	}
	return nil
}

func (r *Runner) composeFinal(ctx context.Context, meta store.RunMeta, plan *texture.Plan, chunks []texture.Chunk) (*texture.Image, error) {
	loaded, err := r.store.LoadChunks(ctx, meta)
	if err != nil {
		return nil, err
	}
	if len(loaded) != len(chunks) {
		return nil, errf(texture.CodeStore, "$.checkpoint", "chunks", "expected %d persisted chunks, got %d", len(chunks), len(loaded))
	}
	last := len(plan.Entries) - 1
	out, err := texture.NewImage(meta.Width, meta.Height, plan.Entries[last].TargetBitDepth, plan.Entries[last].TargetChannels)
	if err != nil {
		return nil, err
	}
	channels := len(out.Layout)
	for i, commit := range loaded {
		if i >= len(chunks) || commit.ChunkX != chunks[i].X || commit.ChunkY != chunks[i].Y {
			return nil, errf(texture.CodeStore, "$.checkpoint", "order", "unstable chunk order")
		}
		stage := commit.Stages[last]
		im := stage.Image
		if im.Width != chunks[i].Rect.Width || im.Height != chunks[i].Rect.Height ||
			im.BitDepth != out.BitDepth || texture.LayoutString(im.Layout) != texture.LayoutString(out.Layout) {
			return nil, errf(texture.CodeStore, "$.checkpoint", "image", "final stage chunk metadata mismatch")
		}
		for y := 0; y < im.Height; y++ {
			dstStart := ((chunks[i].Rect.Y+y)*out.Width + chunks[i].Rect.X) * channels
			copy(out.Pix[dstStart:dstStart+im.Width*channels], im.Pix[y*im.Width*channels:(y+1)*im.Width*channels])
		}
	}
	return out, nil
}

type digestBuilder struct {
	h interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
}

func newDigestBuilder() *digestBuilder { return &digestBuilder{h: sha256.New()} }

func (d *digestBuilder) Add(ref CommitRef, stage store.StageOutput) {
	fmt.Fprintf(d.h, "%d:%d:%d:%d:%s:%s:%d:%d:%s\n",
		ref.CommitSeq, ref.ChunkX, ref.ChunkY, stage.EntryIndex,
		stage.CacheKey, stage.OutputHash, stage.Image.Width, stage.Image.Height,
		texture.LayoutString(stage.Image.Layout))
}

func (d *digestBuilder) Sum() string {
	return hex.EncodeToString(d.h.Sum(nil))
}

func digestValues(builders []*digestBuilder) []StageDigest {
	out := make([]StageDigest, len(builders))
	for i, b := range builders {
		out[i] = StageDigest{EntryIndex: i, Digest: b.Sum()}
	}
	return out
}

func snapshotFor(stage store.StageOutput, plan *texture.Plan, commit store.ChunkCommit, chunkIndex int) texture.FrozenOutput {
	return texture.FrozenOutput{
		EntryIndex: stage.EntryIndex, ChunkIndex: chunkIndex,
		ChunkX: commit.ChunkX, ChunkY: commit.ChunkY,
		OutputHash: stage.OutputHash, CacheKey: stage.CacheKey,
		ParameterHash:     texture.EntryParameterHash(plan.Entries[stage.EntryIndex]),
		ParameterSnapshot: texture.EntryParameterSnapshot(plan.Entries[stage.EntryIndex]),
		PlanFingerprint:   plan.Fingerprint, PlanVersion: plan.Version,
	}
}

func snapshotForStage(stage store.StageOutput, plan *texture.Plan, commit store.ChunkCommit, chunkIndex int) texture.FrozenOutput {
	return snapshotFor(stage, plan, commit, chunkIndex)
}
