package runtime

import (
	"context"
	"sort"
	"sync"

	"samplingkernel/internal/errs"
	"samplingkernel/internal/merge"
	"samplingkernel/internal/model"
)

const (
	MinWorkers        = 1
	MaxAllowedWorkers = 64
	MinTraceBuffer    = 1
	MaxAllowedBuffer  = 4096
)

type Options struct {
	Workers   int `json:"workers"`
	BufferCap int `json:"buffer_cap"`
}

func (o Options) Normalized() Options {
	if o.Workers == 0 {
		o.Workers = 4
	}
	if o.BufferCap == 0 {
		o.BufferCap = 128
	}
	return o
}

func ValidateOptions(options Options) (Options, error) {
	if options.Workers < MinWorkers || options.Workers > MaxAllowedWorkers {
		return options, errs.New(errs.RuntimeWorkerBudgetBad, "worker budget is out of declared bounds").
			AtPath("$.runtime.workers").
			WithDetail("workers=" + itoa(options.Workers) + " allowed=1..64")
	}
	if options.BufferCap < MinTraceBuffer || options.BufferCap > MaxAllowedBuffer {
		return options, errs.New(errs.RuntimeBufferBudgetBad, "per-trace buffer budget is out of declared bounds").
			AtPath("$.runtime.per_trace_buffer").
			WithDetail("buffer=" + itoa(options.BufferCap) + " allowed=1..4096")
	}
	return options, nil
}

type Scheduler struct {
	options Options
	sem     chan struct{}

	mu      sync.Mutex
	buffers map[string][]model.Span
	results map[string]*model.MergeResult
}

func NewScheduler(options Options) (*Scheduler, error) {
	options, err := ValidateOptions(options)
	if err != nil {
		return nil, err
	}
	return &Scheduler{
		options: options,
		sem:     make(chan struct{}, options.Workers),
		buffers: map[string][]model.Span{},
		results: map[string]*model.MergeResult{},
	}, nil
}

func (s *Scheduler) Options() Options { return s.options }

func (s *Scheduler) SubmitSpan(span model.Span) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	buffer := s.buffers[span.TraceID]
	if len(buffer) >= s.options.BufferCap {
		return errs.New(errs.BackpressureBuffer, "per-trace merge buffer is full; retry after drain").
			AtPath("$.events.span").
			WithDetail("trace_id=" + span.TraceID + " cap=" + itoa(s.options.BufferCap) + " span_id=" + span.SpanID)
	}
	s.buffers[span.TraceID] = append(buffer, span)
	return nil
}

func (s *Scheduler) BufferedSpanCount(traceID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buffers[traceID])
}

func (s *Scheduler) TryAcquireWorker() (func(), error) {
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, nil
	default:
		return nil, errs.New(errs.BackpressureWorkers, "worker budget exhausted; retry after a worker finishes").
			AtPath("$.runtime.workers").
			WithDetail("workers=" + itoa(s.options.Workers))
	}
}

func (s *Scheduler) TryRunTrace(traceID, orphanPolicy string) (*model.MergeResult, error) {
	release, err := s.TryAcquireWorker()
	if err != nil {
		return nil, err
	}
	defer release()
	return s.runTrace(traceID, orphanPolicy, true), nil
}

func (s *Scheduler) MergeCurrent(traceID, orphanPolicy string) *model.MergeResult {
	return s.runTrace(traceID, orphanPolicy, false)
}

func (s *Scheduler) runTrace(traceID, orphanPolicy string, drain bool) *model.MergeResult {
	s.mu.Lock()
	spans := append([]model.Span{}, s.buffers[traceID]...)
	if drain {
		delete(s.buffers, traceID)
	}
	s.mu.Unlock()
	result := merge.MergeTrace(traceID, spans, orphanPolicy)
	s.mu.Lock()
	s.results[traceID] = result
	s.mu.Unlock()
	return result
}

func (s *Scheduler) Result(traceID string) *model.MergeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results[traceID]
}

func (s *Scheduler) RunAllParallel(ctx context.Context, orphanPolicy string) (map[string]*model.MergeResult, error) {
	s.mu.Lock()
	traceIDs := make([]string, 0, len(s.buffers))
	for traceID := range s.buffers {
		traceIDs = append(traceIDs, traceID)
	}
	s.mu.Unlock()
	sort.Strings(traceIDs)

	jobs := make(chan string)
	results := map[string]*model.MergeResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for worker := 0; worker < s.options.Workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for traceID := range jobs {
				result := s.runTrace(traceID, orphanPolicy, true)
				mu.Lock()
				results[traceID] = result
				mu.Unlock()
			}
		}()
	}
	for _, traceID := range traceIDs {
		select {
		case jobs <- traceID:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()

	s.mu.Lock()
	for id, result := range results {
		s.results[id] = result
	}
	s.mu.Unlock()
	return results, nil
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
