package probe

import (
	"context"
	"sync"
	"time"

	"lightprobe/internal/scene"
)

type Job struct {
	Seq               int
	ProbeID           string
	RoomID            string
	I, J, K           int
	Position          [3]float64
	InputRadiance     [3]float64
	VisibilitySamples int
	PhotonGrid        scene.PhotonGrid
	SHCost            int
}

type Result struct {
	Seq        int
	ProbeID    string
	RoomID     string
	Radiance   [3]float64
	SH         [9][3]int64
	PhotonCell [3]int
	SHDigest   string
	ErrCode    string
	ErrMessage string
}

type Metrics struct {
	mu          sync.Mutex
	queueEvents []QueueEvent
}

func (m *Metrics) record(event QueueEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueEvents = append(m.queueEvents, event)
}

func (m *Metrics) Events() []QueueEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]QueueEvent, len(m.queueEvents))
	copy(out, m.queueEvents)
	return out
}

func (m *Metrics) appendEvents(events []QueueEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueEvents = append(m.queueEvents, events...)
}

type weightedSemaphore struct {
	mu        sync.Mutex
	cond      *sync.Cond
	available int
}

func newWeightedSemaphore(initial int) *weightedSemaphore {
	s := &weightedSemaphore{available: initial}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *weightedSemaphore) tryAcquire(cost int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.available >= cost {
		s.available -= cost
		return true
	}
	return false
}

func (s *weightedSemaphore) acquire(cost int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.available < cost {
		s.cond.Wait()
	}
	s.available -= cost
}

func (s *weightedSemaphore) release(cost int) {
	s.mu.Lock()
	s.available += cost
	s.mu.Unlock()
	s.cond.Broadcast()
}

type scheduledTask struct {
	seq int
	job Job
}

type orderedResult struct {
	seq    int
	result Result
}

func runBoundedJobs(
	ctx context.Context,
	cfg Config,
	metrics *Metrics,
	jobs []Job,
	execute func(context.Context, Job) Result,
	commit func(Result),
) []QueueEvent {
	cfg = cfg.Normalize()
	localEvents := make([]QueueEvent, 0)
	var eventMu sync.Mutex
	record := func(event QueueEvent) {
		if metrics != nil {
			metrics.record(event)
		}
		eventMu.Lock()
		localEvents = append(localEvents, event)
		localEvents[len(localEvents)-1] = event
		eventMu.Unlock()
	}

	workerCount := cfg.MaxParallelism
	bufferCapacity := cfg.ProbeBufferPerWorker - 1
	if bufferCapacity < 0 {
		bufferCapacity = 0
	}
	workerChans := make([]chan scheduledTask, workerCount)
	for i := range workerChans {
		workerChans[i] = make(chan scheduledTask, bufferCapacity)
	}

	shSem := newWeightedSemaphore(cfg.SHBufferCoeffBudget)
	results := make(chan orderedResult, len(jobs))
	var workers sync.WaitGroup
	for workerID := 0; workerID < workerCount; workerID++ {
		workers.Add(1)
		go func(workerID int) {
			defer workers.Done()
			for task := range workerChans[workerID] {
				cost := task.job.SHCost
				if cost <= 0 {
					cost = 27
				}
				if !shSem.tryAcquire(cost) {
					start := time.Now()
					if cfg.beforeBudgetWait != nil {
						cfg.beforeBudgetWait("sh_temp_buffer", task.job.ProbeID)
					}
					shSem.acquire(cost)
					wait := time.Since(start)
					record(QueueEvent{
						ProbeID: task.job.ProbeID, Worker: workerID,
						Resource: "sh_temp_buffer", Reason: "sh_buffer_budget_exhausted",
						WaitNanos: wait.Nanoseconds(), WaitMS: float64(wait.Microseconds()) / 1000.0,
						Detail: "waiting for fixed-point SH coefficient temp buffer",
					})
				}
				result := execute(ctx, task.job)
				shSem.release(cost)
				results <- orderedResult{seq: task.seq, result: result}
			}
		}(workerID)
	}

	go func() {
		for seq, job := range jobs {
			task := scheduledTask{seq: seq, job: job}
			preferred := seq % workerCount
			workerID := preferred
			select {
			case workerChans[workerID] <- task:
				continue
			default:
			}

			start := time.Now()
			select {
			case workerChans[workerID] <- task:
			case <-ctx.Done():
				result := Result{Seq: seq, ProbeID: job.ProbeID, RoomID: job.RoomID,
					ErrCode: "E_STATE", ErrMessage: ctx.Err().Error()}
				results <- orderedResult{seq: seq, result: result}
				continue
			}
			wait := time.Since(start)
			record(QueueEvent{
				ProbeID: job.ProbeID, Worker: workerID,
				Resource: "probe_buffer", Reason: "per_worker_probe_buffer_full",
				WaitNanos: wait.Nanoseconds(), WaitMS: float64(wait.Microseconds()) / 1000.0,
				Detail: "all per-worker probe buffers are occupied; task is queued",
			})
		}
		for _, ch := range workerChans {
			close(ch)
		}
		go func() {
			workers.Wait()
			close(results)
		}()
	}()

	pending := make(map[int]Result)
	next := 0
	for item := range results {
		pending[item.seq] = item.result
		for {
			result, ok := pending[next]
			if !ok {
				break
			}
			commit(result)
			delete(pending, next)
			next++
		}
	}

	eventMu.Lock()
	out := make([]QueueEvent, len(localEvents))
	copy(out, localEvents)
	eventMu.Unlock()
	return out
}
