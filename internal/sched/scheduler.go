package sched

import (
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"qkernel/internal/errs"
)

type QueueEvent struct {
	UnitID     string `json:"unit_id"`
	Reason     string `json:"reason"`
	WaitedMS   int64  `json:"waited_ms"`
	AtUnixNano int64  `json:"at_unix_nano"`
}

type Unit struct {
	ID           string
	BufferWeight int
	Work         func() error
}

type Result struct {
	UnitID string
	Err    error
}

type Scheduler struct {
	Workers              int
	BufferBytesPerWorker int

	mu     sync.Mutex
	events []QueueEvent
}

func New(workers, bufferBytesPerWorker int) *Scheduler {
	if workers <= 0 {
		workers = 1
	}
	if bufferBytesPerWorker <= 0 {
		bufferBytesPerWorker = 4 << 20
	}
	return &Scheduler{Workers: workers, BufferBytesPerWorker: bufferBytesPerWorker}
}

func (s *Scheduler) Events() []QueueEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]QueueEvent(nil), s.events...)
}

func (s *Scheduler) record(e QueueEvent) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *Scheduler) Run(units []Unit) []Result {
	results := make([]Result, len(units))
	if len(units) == 0 {
		return results
	}
	workerSlots := make(chan struct{}, s.Workers)
	budget := newBufferBudget(s.Workers*s.BufferBytesPerWorker, s.record)
	var wg sync.WaitGroup

	for i := range units {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := units[i]
			select {
			case workerSlots <- struct{}{}:
			default:
				waitStart := time.Now()
				workerSlots <- struct{}{}
				s.record(QueueEvent{UnitID: u.ID, Reason: "max_parallelism", WaitedMS: time.Since(waitStart).Milliseconds(), AtUnixNano: time.Now().UnixNano()})
			}
			defer func() { <-workerSlots }()

			lease, err := budget.Take(u.ID, u.BufferWeight)
			if err != nil {
				results[i] = Result{UnitID: u.ID, Err: err}
				return
			}
			defer lease.Release()

			defer func() {
				if r := recover(); r != nil {
					results[i] = Result{UnitID: u.ID, Err: &errs.Error{
						Code:    errs.EBuild,
						Message: fmt.Sprintf("unit panic: %v\n%s", r, debug.Stack()),
					}}
				}
			}()
			if u.Work == nil {
				results[i] = Result{UnitID: u.ID, Err: errs.New(errs.EBuild, "unit work is nil")}
				return
			}
			if err := u.Work(); err != nil {
				results[i] = Result{UnitID: u.ID, Err: err}
				return
			}
			results[i] = Result{UnitID: u.ID}
		}()
	}
	wg.Wait()
	return results
}

type bufferBudget struct {
	capacity  int
	available int
	mu        sync.Mutex
	cond      *sync.Cond
	record    func(QueueEvent)
}

func newBufferBudget(capacity int, record func(QueueEvent)) *bufferBudget {
	b := &bufferBudget{capacity: capacity, available: capacity, record: record}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *bufferBudget) Take(unitID string, weight int) (*bufferLease, error) {
	if weight <= 0 {
		weight = 1
	}
	b.mu.Lock()
	if weight > b.capacity {
		b.mu.Unlock()
		return nil, errs.New(errs.EPara, "pixel buffer weight %d exceeds total budget %d", weight, b.capacity)
	}
	waited := false
	start := time.Now()
	for b.available < weight {
		if !waited {
			waited = true
		}
		b.cond.Wait()
	}
	b.available -= weight
	b.mu.Unlock()
	if waited && b.record != nil {
		b.record(QueueEvent{UnitID: unitID, Reason: "buffer_exhausted", WaitedMS: time.Since(start).Milliseconds(), AtUnixNano: time.Now().UnixNano()})
	}
	return &bufferLease{b: b, weight: weight}, nil
}

type bufferLease struct {
	b      *bufferBudget
	weight int
	once   sync.Once
}

func (l *bufferLease) Release() {
	l.once.Do(func() {
		l.b.mu.Lock()
		l.b.available += l.weight
		l.b.cond.Broadcast()
		l.b.mu.Unlock()
	})
}
