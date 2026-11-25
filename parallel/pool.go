package parallel

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	MaxWorkers      int
	QueueBudget     int
	CandidateBuffer int
}

type Work struct {
	ID             string
	CandidateCount int
	Run            func(ctx context.Context, from, to int, last bool) (any, error)
}

type WaitEvent struct {
	WorkID   string        `json:"work_id"`
	Reason   string        `json:"reason"`
	Duration time.Duration `json:"duration_ns"`
}

type Outcome struct {
	ID            string        `json:"id"`
	Value         any           `json:"-"`
	Err           error         `json:"-"`
	Chunks        int           `json:"chunks"`
	QueueWait     time.Duration `json:"queue_wait_ns"`
	CandidateWait time.Duration `json:"candidate_wait_ns"`
	index         int
}

type Stats struct {
	Submitted          int64         `json:"submitted"`
	Completed          int64         `json:"completed"`
	Failed             int64         `json:"failed"`
	QueueWaits         int64         `json:"queue_waits"`
	QueueWaitTotal     time.Duration `json:"queue_wait_total_ns"`
	CandidateWaits     int64         `json:"candidate_buffer_waits"`
	CandidateWaitTotal time.Duration `json:"candidate_buffer_wait_total_ns"`
	MaxRunning         int64         `json:"max_running"`
}

type job struct {
	work     Work
	index    int
	enqueued time.Time
	resultCh chan<- workerResult
}

type workerResult struct {
	outcome Outcome
	events  []WaitEvent
}

// Pool is a bounded worker pool. Submission blocks when the frame queue is
// full; candidate chunks larger than CandidateBuffer are serialized in chunks
// and the backpressure interval is recorded instead of dropping work.
type Pool struct {
	config  Config
	jobs    chan job
	stop    chan struct{}
	wg      sync.WaitGroup
	jobWG   sync.WaitGroup
	mu      sync.Mutex
	stats   Stats
	events  []WaitEvent
	running int64
}

func New(config Config) (*Pool, error) {
	if config.MaxWorkers < 1 {
		return nil, fmt.Errorf("parallel: MaxWorkers must be at least 1")
	}
	if config.QueueBudget < 1 {
		return nil, fmt.Errorf("parallel: QueueBudget must be at least 1")
	}
	if config.CandidateBuffer < 1 {
		return nil, fmt.Errorf("parallel: CandidateBuffer must be at least 1")
	}
	pool := &Pool{
		config: config, jobs: make(chan job, config.QueueBudget), stop: make(chan struct{}),
	}
	for i := 0; i < config.MaxWorkers; i++ {
		pool.wg.Add(1)
		go pool.worker()
	}
	return pool, nil
}

func (p *Pool) Execute(ctx context.Context, works []Work) ([]Outcome, Stats, error) {
	results := make([]workerResult, len(works))
	resultCh := make(chan workerResult, len(works))
	var producer sync.WaitGroup
	producer.Add(1)
	go func() {
		defer producer.Done()
		for index, work := range works {
			item := job{work: work, index: index, enqueued: time.Now(), resultCh: resultCh}
			p.jobWG.Add(1)
			sent := false
			select {
			case p.jobs <- item:
				sent = true
				p.addSubmitted()
			case <-ctx.Done():
			default:
				start := time.Now()
				select {
				case p.jobs <- item:
					sent = true
					wait := time.Since(start)
					p.recordWait(WaitEvent{WorkID: work.ID, Reason: "frame_queue_budget", Duration: wait})
					p.addSubmitted()
				case <-ctx.Done():
				}
			}
			if !sent {
				p.jobWG.Done()
				later := works[index:]
				for offset := range later {
					resultCh <- workerResult{outcome: Outcome{
						ID: later[offset].ID, Err: ctx.Err(), index: index + offset,
					}}
				}
				return
			}
		}
	}()
	go func() {
		producer.Wait()
		p.jobWG.Wait()
		close(resultCh)
	}()
	var firstErr error
	for result := range resultCh {
		results[result.outcome.index] = result
		if result.outcome.Err != nil && firstErr == nil {
			firstErr = result.outcome.Err
		}
	}
	outcomes := make([]Outcome, len(works))
	for i := range results {
		outcomes[i] = results[i].outcome
	}
	return outcomes, p.SnapshotStats(), firstErr
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for item := range p.jobs {
		select {
		case <-p.stop:
			return
		default:
		}
		queueWait := time.Since(item.enqueued)
		if queueWait > 0 {
			p.recordWait(WaitEvent{WorkID: item.work.ID, Reason: "worker_dispatch", Duration: queueWait})
		}
		running := atomic.AddInt64(&p.running, 1)
		p.noteMaxRunning(running)
		outcome, events := p.runWork(item, queueWait)
		atomic.AddInt64(&p.running, -1)
		p.recordWorkerResult(outcome, events)
		item.resultCh <- workerResult{outcome: outcome, events: events}
		p.jobWG.Done()
	}
}

func (p *Pool) runWork(item job, queueWait time.Duration) (Outcome, []WaitEvent) {
	outcome := Outcome{ID: item.work.ID, QueueWait: queueWait, index: item.index}
	var events []WaitEvent
	candidateBuffer := p.config.CandidateBuffer
	chunks := 1
	if item.work.CandidateCount > 0 {
		chunks = (item.work.CandidateCount + candidateBuffer - 1) / candidateBuffer
	}
	outcome.Chunks = chunks
	var last any
	var bufferHeldSince time.Time
	for chunk := 0; chunk < chunks; chunk++ {
		if chunk > 0 {
			wait := time.Since(bufferHeldSince)
			outcome.CandidateWait += wait
			events = append(events, WaitEvent{WorkID: item.work.ID, Reason: "candidate_buffer", Duration: wait})
		}
		bufferHeldSince = time.Now()
		from := chunk * candidateBuffer
		to := from + candidateBuffer
		if to > item.work.CandidateCount {
			to = item.work.CandidateCount
		}
		value, err := item.work.Run(context.Background(), from, to, chunk == chunks-1)
		if err != nil {
			outcome.Err = err
			return outcome, events
		}
		if value != nil {
			last = value
		}
	}
	outcome.Value = last
	return outcome, events
}

func (p *Pool) recordWorkerResult(outcome Outcome, events []WaitEvent) {
	p.mu.Lock()
	p.stats.Completed++
	if outcome.Err != nil {
		p.stats.Failed++
	}
	p.events = append(p.events, events...)
	for _, event := range events {
		if event.Reason == "candidate_buffer" {
			p.stats.CandidateWaits++
			p.stats.CandidateWaitTotal += event.Duration
		}
	}
	p.mu.Unlock()
}

func (p *Pool) addSubmitted() {
	p.mu.Lock()
	p.stats.Submitted++
	p.mu.Unlock()
}

func (p *Pool) recordWait(event WaitEvent) {
	if event.Duration <= 0 {
		event.Duration = time.Nanosecond
	}
	p.mu.Lock()
	p.stats.QueueWaits++
	p.stats.QueueWaitTotal += event.Duration
	p.events = append(p.events, event)
	p.mu.Unlock()
}

func (p *Pool) noteMaxRunning(running int64) {
	p.mu.Lock()
	if running > p.stats.MaxRunning {
		p.stats.MaxRunning = running
	}
	p.mu.Unlock()
}

func (p *Pool) SnapshotStats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

func (p *Pool) WaitEvents() []WaitEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]WaitEvent(nil), p.events...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].WorkID != out[j].WorkID {
			return out[i].WorkID < out[j].WorkID
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

func (p *Pool) Close() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	close(p.jobs)
	p.wg.Wait()
}
