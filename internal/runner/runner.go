package runner

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"semanticshading/internal/apierr"
	"semanticshading/internal/bounds"
	"semanticshading/internal/checkpoint"
	"semanticshading/internal/eval"
	"semanticshading/internal/model"
	"semanticshading/internal/sample"
)

type Config struct {
	Parallelism            int
	QueueBudget            int
	ThreadPixelBufferLimit int
	MaxCommits             int
	BandWidth              int
	ExplicitBand           bool
	FailurePolicy          *FailurePolicy
	BeforeEvaluate         func(blockIndex int)
	OnQueueFull            func(blockIndex int)
}

type FailurePolicy struct {
	mu        sync.Mutex
	FailTimes map[int]int
}

func (p *FailurePolicy) ShouldFail(blockIndex int) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.FailTimes == nil || p.FailTimes[blockIndex] <= 0 {
		return false
	}
	p.FailTimes[blockIndex]--
	return true
}

type QueueWait struct {
	BlockIndex       int    `json:"block_index"`
	Reason           string `json:"reason"`
	WaitNanos        int64  `json:"wait_nanos"`
	QueueBudget      int    `json:"queue_budget"`
	QueueDepthBefore int    `json:"queue_depth_before"`
}

type FailedBlock struct {
	BlockIndex int              `json:"block_index"`
	Block      model.BlockCoord `json:"block"`
	Attempts   int              `json:"attempts"`
	Error      *apierr.Error    `json:"error"`
}

type Report struct {
	StartIndex      int                   `json:"start_index"`
	TotalBlocks     int                   `json:"total_blocks"`
	CommittedCount  int                   `json:"committed_count"`
	CommitSequence  []int                 `json:"commit_sequence"`
	States          []BlockState          `json:"states"`
	QueueWaits      []QueueWait           `json:"queue_waits"`
	FailedBlocks    []FailedBlock         `json:"failed_blocks,omitempty"`
	PeakQueueDepth  int                   `json:"peak_queue_depth"`
	PeakParallelism int                   `json:"peak_parallelism"`
	Parallelism     int                   `json:"parallelism"`
	QueueBudget     int                   `json:"queue_budget"`
	BufferPixels    int                   `json:"buffer_pixels"`
	Paused          bool                  `json:"paused"`
	ResetInfo       *checkpoint.ResetInfo `json:"reset_info,omitempty"`
}

type outcome struct {
	index  int
	record model.BlockRecord
	err    *apierr.Error
}

func ValidateConfig(m model.Manifest, cfg Config) (Config, int, *apierr.Error) {
	if cfg.Parallelism == 0 {
		cfg.Parallelism = 4
	}
	if cfg.Parallelism < 1 || cfg.Parallelism > 64 {
		return cfg, 0, apierr.New(apierr.EPara, "parallelism", "parallelism must be in [1,64], got %d", cfg.Parallelism).
			WithDetail("valid_range", [2]int{1, 64})
	}
	if cfg.QueueBudget == 0 {
		cfg.QueueBudget = 4
	}
	if cfg.QueueBudget < 1 || cfg.QueueBudget > 4096 {
		return cfg, 0, apierr.New(apierr.EPara, "queue_budget", "queue_budget must be in [1,4096], got %d", cfg.QueueBudget).
			WithDetail("valid_range", [2]int{1, 4096})
	}
	band := cfg.BandWidth
	if band == 0 && !cfg.ExplicitBand {
		band = m.BandWidth
		cfg.BandWidth = band
	}
	requiredBuffer := (m.TileSize + 2*band) * (m.TileSize + 2*band)
	corePixels := m.TileSize * m.TileSize
	if requiredBuffer < corePixels {
		requiredBuffer = corePixels
	}
	if cfg.ThreadPixelBufferLimit == 0 {
		cfg.ThreadPixelBufferLimit = 16384
	}
	if cfg.ThreadPixelBufferLimit < requiredBuffer {
		return cfg, requiredBuffer, apierr.New(apierr.EPara, "thread_pixel_buffer_limit",
			"per-thread pixel buffer limit %d pixels is below required padded block buffer %d pixels", cfg.ThreadPixelBufferLimit, requiredBuffer).
			WithDetail("required_pixels", requiredBuffer).
			WithDetail("bytes_rgba", requiredBuffer*m.Channels)
	}
	if cfg.ThreadPixelBufferLimit > 64*1024*1024 {
		return cfg, requiredBuffer, apierr.New(apierr.EPara, "thread_pixel_buffer_limit",
			"thread_pixel_buffer_limit must be <= 67108864, got %d", cfg.ThreadPixelBufferLimit)
	}
	return cfg, requiredBuffer, nil
}

func IdentityFor(s *sample.Sample, truthFingerprint string) checkpoint.Identity {
	return checkpoint.Identity{
		InputHash:        s.InputHash,
		ManifestHash:     s.ManifestHash,
		TruthFingerprint: truthFingerprint,
		ParamVersion:     s.Manifest.ParamVersion,
		AlgoVersion:      s.Manifest.AlgoVersion,
	}
}

func Run(parent context.Context, s *sample.Sample, store *checkpoint.Store, cfg Config, resetInfo *checkpoint.ResetInfo) (*Report, error) {
	if parent == nil {
		parent = context.Background()
	}
	if store == nil {
		return nil, apierr.New(apierr.EStage, "store", "checkpoint store is required")
	}
	normalized, bufferPixels, paraErr := ValidateConfig(s.Manifest, cfg)
	if paraErr != nil {
		return nil, paraErr
	}
	cfg = normalized
	total := s.Manifest.BlockCount()
	start, err := store.Count()
	if err != nil {
		return nil, err
	}
	sequence, err := store.CommitSequence()
	if err != nil {
		return nil, err
	}
	for i, blockIndex := range sequence {
		if blockIndex != i {
			return nil, apierr.New(apierr.EState, "commit_sequence", "checkpoint sequence is not contiguous at position %d: block %d", i, blockIndex)
		}
	}
	if start > total {
		return nil, apierr.New(apierr.EState, "committed_count", "store has %d blocks but manifest declares %d", start, total)
	}

	runCtx, cancel := context.WithCancel(parent)
	defer cancel()

	machines := make([]*Machine, total)
	states := make([]BlockState, total)
	for i := 0; i < total; i++ {
		machines[i] = NewMachine(s.Manifest.Block(i))
		if i < start {
			_ = machines[i].Transition(StateProcessing)
			_ = machines[i].Transition(StateCommitted)
		}
		states[i] = machines[i].State()
	}

	jobs := make(chan int, cfg.QueueBudget)
	outcomes := make(chan outcome, total-start)
	queueWaits := make([]QueueWait, 0, total-start)
	var activeWorkers atomic.Int64
	var peakParallel atomic.Int64

	var wg sync.WaitGroup
	for w := 0; w < cfg.Parallelism; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if runCtx.Err() != nil {
					_ = machines[index].Transition(StatePending)
					continue
				}
				current := activeWorkers.Add(1)
				for {
					peak := peakParallel.Load()
					if current <= peak || peakParallel.CompareAndSwap(peak, current) {
						break
					}
				}
				block := s.Manifest.Block(index)
				if transitionErr := machines[index].Transition(StateProcessing); transitionErr != nil {
					outcomes <- outcome{index: index, err: transitionErr}
					activeWorkers.Add(-1)
					continue
				}
				if cfg.BeforeEvaluate != nil {
					cfg.BeforeEvaluate(index)
				}
				var ae *apierr.Error
				if cfg.FailurePolicy.ShouldFail(index) {
					ae = apierr.New(apierr.EStage, "block_evaluator", "injected single-block internal failure after %d attempt(s)", machines[index].info.Attempts).
						WithBlock(block.BX, block.BY).
						WithDetail("block_index", index).
						WithDetail("retryable", true)
				} else {
					record := eval.EvaluateBlock(s, index, cfg.BandWidth)
					if record.MissingEdges > 0 || !record.BandSufficient {
						ae = apierr.New(apierr.EBand, "boundary_jump_q8",
							"band_width=%d is below required_band=%d; missing_edges=%d; band_sufficient=%v",
							cfg.BandWidth, record.RequiredBand, record.MissingEdges, record.BandSufficient).
							WithBlock(block.BX, block.BY).
							WithDetail("block_index", index).
							WithDetail("missing_edges", record.MissingEdges).
							WithDetail("band_sufficient", record.BandSufficient).
							WithDetail("area_field", "sum_distance_q8").
							WithDetail("exceeded_field", "boundary_jump_q8")
					} else {
						outcomes <- outcome{index: index, record: record}
					}
				}
				if ae != nil {
					_ = machines[index].Transition(StateFailed)
					machines[index].info.LastError = ae
					outcomes <- outcome{index: index, err: ae}
				}
				activeWorkers.Add(-1)
			}
		}()
	}

	submitDone := make(chan struct{})
	go func() {
		defer close(submitDone)
		for index := start; index < total; index++ {
			qw := QueueWait{BlockIndex: index, QueueBudget: cfg.QueueBudget, QueueDepthBefore: len(jobs)}
			if len(jobs) == cfg.QueueBudget {
				qw.Reason = "queue_budget_exhausted"
				if cfg.OnQueueFull != nil {
					cfg.OnQueueFull(index)
				}
				started := time.Now()
				select {
				case jobs <- index:
				case <-runCtx.Done():
					qw.Reason = "canceled_while_waiting_for_queue_budget"
				}
				qw.WaitNanos = time.Since(started).Nanoseconds()
			} else {
				qw.Reason = "admitted_with_free_budget"
				select {
				case jobs <- index:
				case <-runCtx.Done():
					qw.Reason = "canceled_while_submitting"
				}
			}
			queueWaits = append(queueWaits, qw)
			if runCtx.Err() != nil {
				break
			}
		}
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(outcomes)
	}()

	_ = submitDone
	results := make(map[int]model.BlockRecord)
	blockErrs := make(map[int]*apierr.Error)
	next := start
	committedThisRun := 0
	paused := false
	var firstErr *apierr.Error

	for out := range outcomes {
		if out.err != nil {
			blockErrs[out.index] = out.err
			if firstErr == nil {
				firstErr = out.err
			}
		} else {
			results[out.index] = out.record
		}
		for {
			if _, bad := blockErrs[next]; bad {
				break
			}
			record, ok := results[next]
			if !ok {
				break
			}
			if cfg.MaxCommits > 0 && committedThisRun >= cfg.MaxCommits {
				paused = true
				cancel()
				break
			}
			cp := makeCheckpoint(s, record)
			if err := store.Commit(runCtx, cp); err != nil {
				ae := apierr.New(apierr.EStage, "sqlite_commit", "ordered block commit failed: %v", err).
					WithBlock(record.Block.BX, record.Block.BY).WithDetail("block_index", next)
				blockErrs[next] = ae
				if firstErr == nil {
					firstErr = ae
				}
				_ = machines[next].Transition(StateFailed)
				machines[next].info.LastError = ae
				break
			}
			if err := machines[next].Transition(StateCommitted); err != nil {
				blockErrs[next] = err
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			delete(results, next)
			committedThisRun++
			next++
			if cfg.MaxCommits > 0 && committedThisRun >= cfg.MaxCommits {
				paused = true
				cancel()
			}
		}
	}
	<-submitDone

	for i := range states {
		states[i] = machines[i].State()
	}
	sequence, err = store.CommitSequence()
	if err != nil {
		return nil, err
	}
	count, err := store.Count()
	if err != nil {
		return nil, err
	}
	failed := make([]FailedBlock, 0)
	for i, state := range states {
		if state.State == StateFailed {
			ae := state.LastError
			failed = append(failed, FailedBlock{BlockIndex: i, Block: s.Manifest.Block(i), Attempts: state.Attempts, Error: ae})
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].BlockIndex < failed[j].BlockIndex })
	peakQueue := 0
	for _, q := range queueWaits {
		depth := q.QueueDepthBefore + 1
		if depth > peakQueue {
			peakQueue = depth
		}
	}
	report := &Report{
		StartIndex:      start,
		TotalBlocks:     total,
		CommittedCount:  count,
		CommitSequence:  sequence,
		States:          states,
		QueueWaits:      queueWaits,
		FailedBlocks:    failed,
		PeakQueueDepth:  peakQueue,
		PeakParallelism: int(peakParallel.Load()),
		Parallelism:     cfg.Parallelism,
		QueueBudget:     cfg.QueueBudget,
		BufferPixels:    bufferPixels,
		Paused:          paused,
		ResetInfo:       resetInfo,
	}
	if firstErr != nil {
		return report, firstErr
	}
	if parent.Err() != nil && cfg.MaxCommits == 0 {
		return report, parent.Err()
	}
	return report, nil
}

func makeCheckpoint(s *sample.Sample, record model.BlockRecord) model.Checkpoint {
	boundList := make([]model.ErrorBound, 0, len(record.RegionLabels))
	for _, label := range record.RegionLabels {
		boundList = append(boundList, bounds.Compute(s.Manifest.QuantGear, s.Manifest.QuantBits, s.Manifest.TileSize, model.FixedFractionBits, label))
	}
	return model.Checkpoint{
		ManifestHash:     s.ManifestHash,
		InputHash:        s.InputHash,
		TruthFingerprint: s.Truth.TruthFingerprint,
		ParamVersion:     s.Manifest.ParamVersion,
		AlgoVersion:      s.Manifest.AlgoVersion,
		Block:            record,
		Bounds:           boundList,
		CommitOrder:      record.Block.Index,
	}
}
