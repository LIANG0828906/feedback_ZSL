package engine

import (
	"context"
	"fmt"
	"sync"

	"uvenhance/internal/apperr"
	"uvenhance/internal/cache"
	"uvenhance/internal/yuv"
)

const (
	defaultWorkers              = 2
	defaultBufferBytesPerWorker = int64(128 << 20)
)

type Options struct {
	ParamVersion         int
	AlignmentVersion     int
	Workers              int
	BufferBytesPerWorker int64
	CancelAfterCommits   int
}

type Invalidation struct {
	Plane    string   `json:"plane"`
	Stage    string   `json:"stage"`
	Reason   string   `json:"reason"`
	Upstream []string `json:"upstream,omitempty"`
}

type Stats struct {
	RecomputeCount       int           `json:"recompute_count"`
	CacheHitCount        int           `json:"cache_hit_count"`
	InvalidationCount    int           `json:"invalidation_count"`
	SnapshotRejected     bool          `json:"snapshot_rejected"`
	SnapshotRejectReason string        `json:"snapshot_reject_reason,omitempty"`
	Workers              int           `json:"workers"`
	BufferBytesPerWorker int64         `json:"buffer_bytes_per_worker"`
	BufferWaitEvents     []BufferEvent `json:"buffer_wait_events"`
}

type RunResult struct {
	Complete      bool                     `json:"complete"`
	SnapshotID    int64                    `json:"snapshot_id,omitempty"`
	SnapshotUID   string                   `json:"snapshot_uid,omitempty"`
	Stages        map[string]*cache.Output `json:"-"`
	Final         *yuv.Image               `json:"-"`
	Invalidations []Invalidation           `json:"invalidations"`
	Recomputed    []string                 `json:"recomputed_stages"`
	CacheHits     []string                 `json:"cache_hit_stages"`
	Committed     []string                 `json:"committed_stages"`
	Stats         Stats                    `json:"stats"`
}

type stageState struct {
	spec        cache.StageSpec
	valid       bool
	started     bool
	done        bool
	out         *cache.Output
	parents     []*cache.Output
	reason      string
	upstream    []string
	key         string
	inputHash   string
	upKey       string
	paramDigest string
	params      yuv.Params
}

type Engine struct {
	store *cache.Store
	mu    sync.Mutex
	live  *liveState
}

func New(store *cache.Store) *Engine { return &Engine{store: store} }

type liveState struct {
	paramVersion     int
	alignmentVersion int
	alignment        string
	upsample         string
	shape            string
	stages           map[string]*cache.Output
}

func (e *Engine) Run(ctx context.Context, sample *yuv.Sample, params yuv.Params, opts Options) (*RunResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.live = nil
	return e.runInternal(ctx, sample, params, opts, nil)
}

// RunLive keeps exact-key outputs in memory between parameter or input-plane edits.
// A fresh process restart still goes through Run and therefore must pass the whole
// snapshot hash/version check before any stage may be reused.
func (e *Engine) RunLive(ctx context.Context, sample *yuv.Sample, params yuv.Params, opts Options) (*RunResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	shape := fmt.Sprintf("%dx%d/%dx%d/%dx%d/%d/%d-%d/%s/%s",
		sample.Y.Width, sample.Y.Height, sample.U.Width, sample.U.Height, sample.V.Width, sample.V.Height,
		sample.Metadata.BitDepth, sample.Metadata.QuantMin, sample.Metadata.QuantMax,
		sample.Metadata.ChromaAlignment, sample.Metadata.Upsample)
	var base map[string]*cache.Output
	if e.live != nil && e.live.paramVersion == opts.ParamVersion &&
		e.live.alignmentVersion == opts.AlignmentVersion && e.live.shape == shape &&
		e.live.alignment == sample.Metadata.ChromaAlignment && e.live.upsample == sample.Metadata.Upsample {
		base = cloneOutputs(e.live.stages)
	}
	result, err := e.runInternal(ctx, sample, params, opts, base)
	if result != nil && (err == nil || apperr.Code(err) == apperr.CodeCanceled) {
		e.live = &liveState{
			paramVersion: opts.ParamVersion, alignmentVersion: opts.AlignmentVersion,
			alignment: sample.Metadata.ChromaAlignment, upsample: sample.Metadata.Upsample,
			shape: shape, stages: cloneOutputs(result.Stages),
		}
	} else if err != nil {
		// A non-cancel error may have left in-memory results that were not
		// atomically persisted. Force the next call back through SQLite.
		e.live = nil
	}
	return result, err
}

func cloneOutputs(in map[string]*cache.Output) map[string]*cache.Output {
	out := make(map[string]*cache.Output, len(in))
	for id, v := range in {
		out[id] = v
	}
	return out
}

func (e *Engine) runInternal(ctx context.Context, sample *yuv.Sample, params yuv.Params, opts Options,
	liveBase map[string]*cache.Output) (*RunResult, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	if opts.ParamVersion <= 0 {
		return nil, apperr.New(apperr.CodeCache, "parameter version must be positive")
	}
	if opts.AlignmentVersion <= 0 {
		return nil, apperr.New(apperr.CodeCache, "alignment version must be positive")
	}
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.Workers > 16 {
		return nil, apperr.Resource("worker count exceeds hard limit 16")
	}
	if opts.BufferBytesPerWorker <= 0 {
		opts.BufferBytesPerWorker = defaultBufferBytesPerWorker
	}

	hashes := map[string]string{
		"Y": sample.Y.ContentHash(),
		"U": sample.U.ContentHash(),
		"V": sample.V.ContentHash(),
	}
	result := &RunResult{Stages: make(map[string]*cache.Output)}
	result.Stats.Workers = opts.Workers
	result.Stats.BufferBytesPerWorker = opts.BufferBytesPerWorker

	base := liveBase
	if base == nil {
		base = make(map[string]*cache.Output)
		loaded, err := e.store.LoadLatest(ctx)
		if err != nil {
			return result, err
		}
		if loaded.RejectedReason != "" {
			result.Stats.SnapshotRejected = true
			result.Stats.SnapshotRejectReason = loaded.RejectedReason
		} else if loaded.Snapshot != nil {
			snap := loaded.Snapshot
			result.SnapshotID = snap.ID
			result.SnapshotUID = snap.UID
			reason := snap.RejectReason(opts.ParamVersion, opts.AlignmentVersion,
				sample.Metadata.ChromaAlignment, sample.Metadata.Upsample, hashes)
			if reason != "" {
				if err := e.store.Reject(ctx, snap.ID); err != nil {
					return result, err
				}
				result.Stats.SnapshotRejected = true
				result.Stats.SnapshotRejectReason = reason
				result.SnapshotID = 0
				result.SnapshotUID = ""
			} else {
				for id, out := range snap.Stages {
					base[id] = out
				}
			}
		}
	}

	states := make(map[string]*stageState, len(cache.Stages))
	for _, spec := range cache.Stages {
		states[spec.ID] = &stageState{spec: spec}
	}

	for _, spec := range cache.Stages {
		st := states[spec.ID]
		if result.Stats.SnapshotRejected {
			st.reason = "snapshot_rejected"
			result.Invalidations = append(result.Invalidations, Invalidation{
				Plane: spec.Plane, Stage: spec.ID, Reason: st.reason,
			})
			continue
		}
		parentsReady := true
		var invalidParents []string
		for _, dep := range spec.Deps {
			if !states[dep].valid {
				parentsReady = false
				invalidParents = append(invalidParents, dep)
			}
		}
		if !parentsReady {
			st.reason = "upstream_changed"
			st.upstream = invalidParents
			result.Invalidations = append(result.Invalidations, Invalidation{
				Plane: spec.Plane, Stage: spec.ID, Reason: st.reason, Upstream: invalidParents,
			})
			continue
		}

		inputHash, upstreamKey, paramValue, err := stageInputs(sample, params, spec, states)
		if err != nil {
			return result, err
		}
		key, paramDigest, err := cache.MakeKey(inputHash, spec.ID, paramValue, depKeys(states, spec.Deps))
		if err != nil {
			return result, err
		}
		st.key, st.inputHash, st.upKey, st.paramDigest = key, inputHash, upstreamKey, paramDigest
		rec := base[spec.ID]
		if rec != nil && rec.Key == key {
			st.valid = true
			st.done = true
			st.out = rec
			result.Stages[spec.ID] = rec
			result.CacheHits = append(result.CacheHits, spec.ID)
			result.Stats.CacheHitCount++
			continue
		}
		reason := invalidationReason(rec, inputHash, upstreamKey, paramDigest)
		st.reason = reason
		result.Invalidations = append(result.Invalidations, Invalidation{
			Plane: spec.Plane, Stage: spec.ID, Reason: reason,
		})
	}
	result.Stats.InvalidationCount = len(result.Invalidations)

	limiter := newBufferLimiter(int64(opts.Workers) * opts.BufferBytesPerWorker)
	computed, canceled, runErr := e.computeInvalid(ctx, sample, params, opts, states, limiter)
	result.Stats.BufferWaitEvents = limiter.snapshotEvents()
	for id, out := range computed {
		states[id].out = out
		states[id].done = true
	}

	persistCtx := context.Background()
	commitCount := 0
	for _, spec := range cache.Stages {
		st := states[spec.ID]
		if st.valid {
			continue
		}
		if st.out == nil {
			continue
		}
		result.Stages[spec.ID] = st.out
		result.Committed = append(result.Committed, spec.ID)
		result.Recomputed = append(result.Recomputed, spec.ID)
		result.Stats.RecomputeCount++
		commitCount++

		complete := len(result.Stages) == len(cache.Stages)
		id, uid, err := e.store.SaveSnapshot(persistCtx, cache.SnapshotMeta{
			ParamVersion: opts.ParamVersion, AlignmentVersion: opts.AlignmentVersion,
			Alignment: sample.Metadata.ChromaAlignment, Upsample: sample.Metadata.Upsample,
			InputHashes: hashes, Complete: complete,
		}, result.Stages)
		if err != nil {
			return result, err
		}
		result.SnapshotID, result.SnapshotUID = id, uid
		result.Complete = complete
		if opts.CancelAfterCommits > 0 && commitCount >= opts.CancelAfterCommits {
			return result, apperr.Canceled(context.Canceled)
		}
	}
	if canceled {
		return result, apperr.Canceled(context.Canceled)
	}
	if runErr != nil {
		return result, runErr
	}
	for _, spec := range cache.Stages {
		if result.Stages[spec.ID] == nil {
			return result, apperr.New(apperr.CodeInvariant, "missing stage after scheduling: "+spec.ID)
		}
	}
	result.Complete = true
	if result.Final = result.Stages[cache.StageInterleave].ImageData; result.Final == nil {
		return result, apperr.New(apperr.CodeInvariant, "interleave stage has no image")
	}
	return result, nil
}

func stageInputs(sample *yuv.Sample, params yuv.Params, spec cache.StageSpec, states map[string]*stageState) (string, string, any, error) {
	switch spec.ID {
	case cache.StageYDenoise:
		return sample.Y.ContentHash(), "", cache.ParamsForStage(params, spec.ID), nil
	case cache.StageUSmooth:
		return sample.U.ContentHash(), "", cache.ParamsForStage(params, spec.ID), nil
	case cache.StageVSmooth:
		return sample.V.ContentHash(), "", cache.ParamsForStage(params, spec.ID), nil
	case cache.StageInterleave:
		parents := []string{cache.StageYContrast, cache.StageUGain, cache.StageVGain}
		hashes := make([]string, 0, len(parents))
		for _, dep := range parents {
			if states[dep].out == nil {
				return "", "", nil, apperr.New(apperr.CodeInvariant, "interleave parent unavailable: "+dep)
			}
			hashes = append(hashes, states[dep].out.OutputHash)
		}
		p := cache.InterleaveParams{
			Upsample: sample.Metadata.Upsample, Alignment: sample.Metadata.ChromaAlignment,
			BitDepth: sample.Metadata.BitDepth, QuantMin: sample.Metadata.QuantMin,
			QuantMax: sample.Metadata.QuantMax,
		}
		return cache.JoinHashes(hashes...), cache.JoinKeys(depKeys(states, spec.Deps)...), p, nil
	default:
		if len(spec.Deps) != 1 {
			return "", "", nil, apperr.New(apperr.CodeInvariant, "unexpected dependencies for "+spec.ID)
		}
		parent := states[spec.Deps[0]].out
		if parent == nil {
			return "", "", nil, apperr.New(apperr.CodeInvariant, "parent output unavailable for "+spec.ID)
		}
		return parent.OutputHash, parent.Key, cache.ParamsForStage(params, spec.ID), nil
	}
}

func depKeys(states map[string]*stageState, deps []string) []string {
	keys := make([]string, 0, len(deps))
	for _, dep := range deps {
		keys = append(keys, states[dep].key)
	}
	return keys
}

func invalidationReason(rec *cache.Output, inputHash, upstreamKey, paramDigest string) string {
	if rec == nil {
		return "cache_missing"
	}
	if rec.InputHash != inputHash {
		return "input_changed"
	}
	if rec.ParamDigest != paramDigest {
		return "parameter_changed"
	}
	if rec.UpstreamKey != upstreamKey {
		return "upstream_changed"
	}
	return "cache_key_mismatch"
}

type stageDone struct {
	id  string
	out *cache.Output
	err error
}

func (e *Engine) computeInvalid(ctx context.Context, sample *yuv.Sample, params yuv.Params, opts Options,
	states map[string]*stageState, limiter *bufferLimiter) (map[string]*cache.Output, bool, error) {
	computed := make(map[string]*cache.Output)
	results := make(chan stageDone, len(cache.Stages))
	running := 0
	pending := 0
	for _, st := range states {
		if !st.valid && !st.done {
			pending++
		}
	}
	canceled := false
	var firstErr error
	var wg sync.WaitGroup

	for {
		if !canceled && firstErr == nil {
			if ctx.Err() != nil {
				canceled = true
			}
		}
		if !canceled && firstErr == nil {
			for _, spec := range cache.Stages {
				if running >= opts.Workers {
					break
				}
				st := states[spec.ID]
				if st.valid || st.started || st.done {
					continue
				}
				ready := true
				for _, dep := range spec.Deps {
					if states[dep].out == nil {
						ready = false
						break
					}
				}
				if !ready {
					continue
				}
				inputHash, upstreamKey, paramValue, err := stageInputs(sample, params, spec, states)
				if err != nil {
					return computed, false, err
				}
				key, paramDigest, err := cache.MakeKey(inputHash, spec.ID, paramValue, depKeys(states, spec.Deps))
				if err != nil {
					return computed, false, err
				}
				st.key, st.inputHash, st.upKey, st.paramDigest = key, inputHash, upstreamKey, paramDigest
				st.params = params
				st.parents = make([]*cache.Output, 0, len(spec.Deps))
				for _, dep := range spec.Deps {
					st.parents = append(st.parents, states[dep].out)
				}
				st.started = true
				running++
				wg.Add(1)
				go func(st *stageState) {
					defer wg.Done()
					out, err := computeOne(ctx, sample, opts, st, limiter)
					results <- stageDone{id: st.spec.ID, out: out, err: err}
				}(st)
			}
		}
		if running == 0 {
			if canceled || firstErr != nil {
				break
			}
			// No runnable task means a dependency was not produced.
			break
		}
		res := <-results
		running--
		st := states[res.id]
		if res.err != nil {
			if firstErr == nil {
				firstErr = res.err
			}
			if apperr.Code(res.err) == apperr.CodeCanceled {
				canceled = true
			}
			continue
		}
		st.out = res.out
		st.done = true
		computed[res.id] = res.out
		pending--
		if ctx.Err() != nil {
			canceled = true
		}
	}
	wg.Wait()
	if !canceled && firstErr == nil {
		for _, st := range states {
			if !st.valid && !st.done {
				return computed, false, apperr.New(apperr.CodeInvariant, "stage dependency cycle or unavailable stage: "+st.spec.ID)
			}
		}
	}
	if firstErr != nil && apperr.Code(firstErr) != apperr.CodeCanceled {
		return computed, canceled, firstErr
	}
	if ctx.Err() != nil {
		canceled = true
	}
	return computed, canceled, nil
}

func computeOne(ctx context.Context, sample *yuv.Sample, opts Options, st *stageState, limiter *bufferLimiter) (*cache.Output, error) {
	need := estimateTaskBytes(st, sample)
	if need > opts.BufferBytesPerWorker {
		limiter.record(BufferEvent{
			Stage: st.spec.ID, Reason: "stage_exceeds_per_worker_buffer_cap",
			Need: need, Capacity: opts.BufferBytesPerWorker,
		})
		return nil, apperr.Resource(fmt.Sprintf("stage %s requires %d bytes, per-worker cap is %d", st.spec.ID, need, opts.BufferBytesPerWorker))
	}
	release, err := limiter.acquire(ctx, st.spec.ID, need)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, apperr.Canceled(err)
	}

	switch st.spec.ID {
	case cache.StageYDenoise:
		p, err := yuv.Denoise(ctx, sample.Y, st.params.Luma.DenoiseStrength)
		if err != nil {
			return nil, wrapCancel(ctx, err)
		}
		return cache.NewPlaneOutput(st.spec.ID, st.key, st.inputHash, st.upKey, st.paramDigest, p), nil
	case cache.StageYSharpen:
		p, err := yuv.Sharpen(ctx, st.parents[0].PlaneData, st.params.Luma.SharpenAmount, st.params.Luma.SharpenThreshold)
		if err != nil {
			return nil, wrapCancel(ctx, err)
		}
		return cache.NewPlaneOutput(st.spec.ID, st.key, st.inputHash, st.upKey, st.paramDigest, p), nil
	case cache.StageYContrast:
		p, err := yuv.LocalContrast(ctx, st.parents[0].PlaneData, st.params.Luma.ContrastStrength, st.params.Luma.ContrastRadius)
		if err != nil {
			return nil, wrapCancel(ctx, err)
		}
		return cache.NewPlaneOutput(st.spec.ID, st.key, st.inputHash, st.upKey, st.paramDigest, p), nil
	case cache.StageUSmooth:
		p, err := yuv.BandLimitedSmooth(ctx, sample.U, st.params.ChromaU.SmoothSigma)
		if err != nil {
			return nil, wrapCancel(ctx, err)
		}
		return cache.NewPlaneOutput(st.spec.ID, st.key, st.inputHash, st.upKey, st.paramDigest, p), nil
	case cache.StageUGain:
		p, err := yuv.ChromaGain(ctx, st.parents[0].PlaneData, st.params.ChromaU.Gain, st.params.ChromaU.Offset)
		if err != nil {
			return nil, wrapCancel(ctx, err)
		}
		return cache.NewPlaneOutput(st.spec.ID, st.key, st.inputHash, st.upKey, st.paramDigest, p), nil
	case cache.StageVSmooth:
		p, err := yuv.BandLimitedSmooth(ctx, sample.V, st.params.ChromaV.SmoothSigma)
		if err != nil {
			return nil, wrapCancel(ctx, err)
		}
		return cache.NewPlaneOutput(st.spec.ID, st.key, st.inputHash, st.upKey, st.paramDigest, p), nil
	case cache.StageVGain:
		p, err := yuv.ChromaGain(ctx, st.parents[0].PlaneData, st.params.ChromaV.Gain, st.params.ChromaV.Offset)
		if err != nil {
			return nil, wrapCancel(ctx, err)
		}
		return cache.NewPlaneOutput(st.spec.ID, st.key, st.inputHash, st.upKey, st.paramDigest, p), nil
	case cache.StageInterleave:
		img, err := yuv.BuildInterleaved(ctx, st.parents[0].PlaneData, st.parents[1].PlaneData, st.parents[2].PlaneData,
			sample.Metadata.ChromaAlignment, sample.Metadata.Upsample)
		if err != nil {
			return nil, wrapCancel(ctx, err)
		}
		return cache.NewImageOutput(st.spec.ID, st.key, st.inputHash, st.upKey, st.paramDigest,
			sample.Metadata.ChromaAlignment, img), nil
	default:
		return nil, apperr.New(apperr.CodeInvariant, "unknown stage "+st.spec.ID)
	}
}

func wrapCancel(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return apperr.Canceled(ctx.Err())
	}
	return err
}

func estimateTaskBytes(st *stageState, sample *yuv.Sample) int64 {
	if st.spec.Kind == cache.KindImage {
		width, height := 0, 0
		for _, p := range st.parents {
			width, height = p.Width, p.Height
			break
		}
		return int64(width) * int64(height) * 18
	}
	var pixels int64
	for _, p := range st.parents {
		pixels += int64(p.Width) * int64(p.Height)
	}
	if pixels == 0 {
		switch st.spec.Plane {
		case "Y":
			pixels = int64(sample.Y.Width) * int64(sample.Y.Height)
		default:
			pixels = int64(sample.U.Width) * int64(sample.U.Height)
		}
	}
	return pixels * 8
}
