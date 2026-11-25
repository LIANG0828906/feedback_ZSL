package audiofp

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
)

type ExtractionOptions struct {
	Workers                  int
	FrameChunk               int
	SamplingBufferPerWorker  int
	MaxSamplingBufferSamples int
	Inject                   *Injection
}

type ExtractionStats struct {
	Workers            int      `json:"workers"`
	Jobs               int      `json:"jobs"`
	Frames             int      `json:"frames"`
	MaxInFlightSamples int      `json:"max_in_flight_samples"`
	QueueWaitEvents    int      `json:"queue_wait_events"`
	QueueEvents        []string `json:"queue_events,omitempty"`
}

func normalizedExtractionOptions(opts ExtractionOptions) ExtractionOptions {
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.Workers > 32 {
		opts.Workers = 32
	}
	if opts.FrameChunk <= 0 {
		opts.FrameChunk = 32
	}
	if opts.SamplingBufferPerWorker <= 0 {
		opts.SamplingBufferPerWorker = 64 * 1024
	}
	return opts
}

type sampleLimiter struct {
	mu       sync.Mutex
	cond     *sync.Cond
	used     int
	capacity int
	peak     int
	events   []string
}

func newSampleLimiter(capacity int) *sampleLimiter {
	l := &sampleLimiter{capacity: capacity}
	l.cond = sync.NewCond(&l.mu)
	return l
}

func (l *sampleLimiter) acquire(ctx context.Context, need int, assetID string, shard, frame int) error {
	if need > l.capacity {
		return parameterError("sampling_buffer_per_worker",
			fmt.Sprintf("a %d-sample frame/span exceeds the %d-sample buffer", need, l.capacity))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	waited := false
	for l.used+need > l.capacity {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		waited = true
		event := fmt.Sprintf("sampling buffer exhausted: need=%d used=%d capacity=%d asset=%s shard=%d frame=%d queued",
			need, l.used, l.capacity, assetID, shard, frame)
		l.events = append(l.events, event)
		l.cond.Wait()
	}
	if waited {
		l.events = append(l.events, fmt.Sprintf("sampling buffer wait released: asset=%s shard=%d frame=%d", assetID, shard, frame))
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	l.used += need
	if l.used > l.peak {
		l.peak = l.used
	}
	return nil
}

func (l *sampleLimiter) release(need int) {
	l.mu.Lock()
	l.used -= need
	l.cond.Broadcast()
	l.mu.Unlock()
}

func (l *sampleLimiter) snapshot() (int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	events := append([]string(nil), l.events...)
	sort.Strings(events)
	return l.peak, events
}

// ExtractAsset extracts every frame, including all-zero frames.
func ExtractAsset(ctx context.Context, asset PCMAsset, r Recipe, opts ExtractionOptions) ([]FrameFingerprint, ExtractionStats, error) {
	if err := ValidateRecipe(r); err != nil {
		return nil, ExtractionStats{}, err
	}
	if len(asset.Samples) < r.FrameLen {
		return nil, ExtractionStats{}, parameterError("frame_len", "asset is shorter than one frame")
	}
	count := frameCount(len(asset.Samples), r)
	return ExtractFrameRange(ctx, asset, r, 0, count, opts)
}

// ExtractFrameRange extracts [firstFrame,lastFrame). Results are always sorted
// by frame number regardless of worker count, chunk size, or scheduling.
func ExtractFrameRange(ctx context.Context, asset PCMAsset, r Recipe, firstFrame, lastFrame int, opts ExtractionOptions) ([]FrameFingerprint, ExtractionStats, error) {
	opts = normalizedExtractionOptions(opts)
	if err := ValidateRecipe(r); err != nil {
		return nil, ExtractionStats{}, err
	}
	totalFrames := frameCount(len(asset.Samples), r)
	if firstFrame < 0 || lastFrame < firstFrame || lastFrame > totalFrames {
		return nil, ExtractionStats{}, parameterError("frame_range", "frame range is outside the asset")
	}
	if firstFrame == lastFrame {
		return []FrameFingerprint{}, ExtractionStats{Workers: opts.Workers}, nil
	}
	if opts.Inject != nil && opts.Inject.Kind == "frame_order" {
		if (opts.Inject.AssetID == "" || opts.Inject.AssetID == asset.ID) &&
			opts.Inject.Frame >= firstFrame && opts.Inject.Frame < lastFrame {
			return nil, ExtractionStats{}, orderError(asset.ID, opts.Inject.Shard, opts.Inject.Frame,
				"injected frame timing inversion")
		}
	}

	capacity := opts.SamplingBufferPerWorker * opts.Workers
	if opts.MaxSamplingBufferSamples > 0 && opts.MaxSamplingBufferSamples < capacity {
		capacity = opts.MaxSamplingBufferSamples
	}
	if capacity < r.FrameLen {
		return nil, ExtractionStats{}, parameterError("sampling_buffer_per_worker",
			fmt.Sprintf("capacity %d is smaller than frame_len %d", capacity, r.FrameLen))
	}

	maxFramesPerJob := opts.FrameChunk
	spanLimitFrames := (capacity-r.FrameLen)/r.FrameShift + 1
	if spanLimitFrames < 1 {
		spanLimitFrames = 1
	}
	if maxFramesPerJob > spanLimitFrames {
		maxFramesPerJob = spanLimitFrames
	}
	if maxFramesPerJob < 1 {
		maxFramesPerJob = 1
	}

	type job struct {
		index int
		first int
		last  int
	}
	var jobs []job
	for f := firstFrame; f < lastFrame; f += maxFramesPerJob {
		end := f + maxFramesPerJob
		if end > lastFrame {
			end = lastFrame
		}
		jobs = append(jobs, job{index: len(jobs), first: f, last: end})
	}

	limiter := newSampleLimiter(capacity)
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wakeDone := make(chan struct{})
	go func() {
		select {
		case <-workerCtx.Done():
			limiter.mu.Lock()
			limiter.cond.Broadcast()
			limiter.mu.Unlock()
		case <-wakeDone:
		}
	}()
	defer close(wakeDone)
	result := make([]FrameFingerprint, lastFrame-firstFrame)
	jobErrs := make([]error, len(jobs))
	workers := opts.Workers
	if workers > len(jobs) {
		workers = len(jobs)
	}
	jobsCh := make(chan job)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobsCh {
				if err := workerCtx.Err(); err != nil {
					jobErrs[j.index] = err
					continue
				}
				span := (j.last-j.first-1)*r.FrameShift + r.FrameLen
				if err := limiter.acquire(workerCtx, span, asset.ID, opts.InjectShard(), j.first); err != nil {
					jobErrs[j.index] = err
					continue
				}
				for frameNo := j.first; frameNo < j.last; frameNo++ {
					if err := workerCtx.Err(); err != nil {
						jobErrs[j.index] = err
						break
					}
					result[frameNo-firstFrame] = fingerprintFrame(asset, r, frameNo)
				}
				limiter.release(span)
			}
		}()
	}
	go func() {
		defer close(jobsCh)
		for _, j := range jobs {
			select {
			case jobsCh <- j:
			case <-workerCtx.Done():
				return
			}
		}
	}()
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, ExtractionStats{}, err
	}
	for _, err := range jobErrs {
		if err != nil {
			return nil, ExtractionStats{}, err
		}
	}
	peak, events := limiter.snapshot()
	return result, ExtractionStats{
		Workers: workers, Jobs: len(jobs), Frames: lastFrame - firstFrame,
		MaxInFlightSamples: peak, QueueWaitEvents: len(events), QueueEvents: events,
	}, nil
}

func (o ExtractionOptions) InjectShard() int {
	if o.Inject == nil {
		return 0
	}
	return o.Inject.Shard
}

func fingerprintFrame(asset PCMAsset, r Recipe, frameNo int) FrameFingerprint {
	energies, total := bandEnergies(asset.Samples, frameNo, r)
	start := FrameStartOffset(frameNo, r)
	levels := make([]uint8, r.Bands)
	zero := total <= 1e-18
	if !zero {
		limit := float64(maxLevel(r.QuantBits))
		for band, energy := range energies {
			v := int(math.Floor((energy/total)*limit + 0.5))
			if v < 0 {
				v = 0
			}
			if v > maxLevel(r.QuantBits) {
				v = maxLevel(r.QuantBits)
			}
			levels[band] = uint8(v)
		}
	}
	packed := PackLevels(levels, r.QuantBits)
	return FrameFingerprint{
		AssetID: asset.ID, FrameNo: frameNo, StartSample: start,
		Bands: r.Bands, BitWidth: r.QuantBits, Levels: levels,
		PackedHex: packed, Zero: zero, Summary: FrameSummary(start, levels, r.QuantBits, zero),
	}
}
