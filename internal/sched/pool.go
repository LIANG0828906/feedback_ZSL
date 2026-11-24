package sched

import (
	"sync"
	"sync/atomic"
	"time"
)

type QueueEvent struct {
	Time           time.Time `json:"time"`
	TaskID         string    `json:"task_id"`
	Reason         string    `json:"reason"`
	WaitedMS       int64     `json:"waited_ms"`
	MaxParallelism int       `json:"max_parallelism"`
	BufferLimit    int       `json:"buffer_limit"`
}

type NamedTask struct {
	ID string
	Fn func() error
}

type Pool struct {
	Workers     int
	BufferLimit int
	queues      []chan job
	eventsMu    sync.Mutex
	Events      []QueueEvent
	rr          uint64
}

type job struct {
	task NamedTask
	done chan error
}

func NewPool(workers, bufferLimit int) *Pool {
	if workers <= 0 {
		workers = 1
	}
	if bufferLimit <= 0 {
		bufferLimit = 1
	}
	p := &Pool{Workers: workers, BufferLimit: bufferLimit}
	p.queues = make([]chan job, workers)
	for i := range p.queues {
		p.queues[i] = make(chan job, bufferLimit)
		go p.worker(p.queues[i])
	}
	return p
}

func (p *Pool) worker(queue <-chan job) {
	for item := range queue {
		item.done <- item.task.Fn()
		close(item.done)
	}
}

func (p *Pool) RecordedEvents() []QueueEvent {
	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()
	return append([]QueueEvent(nil), p.Events...)
}

func (p *Pool) Run(tasks []NamedTask) []error {
	var wg sync.WaitGroup
	errs := make([]error, len(tasks))
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task NamedTask) {
			defer wg.Done()
			errs[i] = p.Invoke(task)
		}(i, task)
	}
	wg.Wait()
	return errs
}

func (p *Pool) Invoke(task NamedTask) error {
	done := make(chan error, 1)
	item := job{task: task, done: done}

	idx := p.tryEnqueue(item)
	if idx >= 0 {
		return <-done
	}

	start := time.Now()
	idx = int(atomic.AddUint64(&p.rr, 1)-1) % p.Workers
	p.queues[idx] <- item
	waited := time.Since(start)
	p.eventsMu.Lock()
	p.Events = append(p.Events, QueueEvent{
		Time:           start,
		TaskID:         task.ID,
		Reason:         "per_worker_sample_buffer_full",
		WaitedMS:       waited.Milliseconds(),
		MaxParallelism: p.Workers,
		BufferLimit:    p.BufferLimit,
	})
	p.eventsMu.Unlock()
	return <-done
}

func (p *Pool) tryEnqueue(item job) int {
	start := int(atomic.AddUint64(&p.rr, 1)-1) % p.Workers
	for offset := 0; offset < p.Workers; offset++ {
		idx := (start + offset) % p.Workers
		select {
		case p.queues[idx] <- item:
			return idx
		default:
		}
	}
	return -1
}
