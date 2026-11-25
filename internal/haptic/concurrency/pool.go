package concurrency

import (
	"sync"
)

type SampleBudget struct {
	mu       sync.Mutex
	cond     *sync.Cond
	capacity int
	used     int
}

func NewSampleBudget(capacity int) *SampleBudget {
	b := &SampleBudget{capacity: capacity}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *SampleBudget) Acquire(samples int) {
	if samples <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.used+samples > b.capacity {
		b.cond.Wait()
	}
	b.used += samples
}

func (b *SampleBudget) Release(samples int) {
	if samples <= 0 {
		return
	}
	b.mu.Lock()
	if samples > b.used {
		b.used = 0
	} else {
		b.used -= samples
	}
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (b *SampleBudget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (b *SampleBudget) Capacity() int { return b.capacity }

type Job struct {
	Name    string
	Samples int
	LaneKey string
	Fn      func() (any, error)
}

type result struct {
	value any
	err   error
}

type Future struct {
	done     chan result
	samples  int
	budget   *SampleBudget
	released sync.Once
}

func (f *Future) Wait() (any, error) {
	r := <-f.done
	return r.value, r.err
}

func (f *Future) Release() {
	f.released.Do(func() { f.budget.Release(f.samples) })
}

type lane struct {
	jobs   chan queuedJob
	budget *SampleBudget
}

type queuedJob struct {
	job  Job
	done chan result
}

type Pool struct {
	maxWorkers    int
	bufferSamples int
	lanes         []*lane
	rr            int
	submitMu      sync.Mutex
}

func NewPool(maxWorkers, bufferSamples int) *Pool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	if bufferSamples < 1 {
		bufferSamples = maxWorkers * 8
	}
	p := &Pool{maxWorkers: maxWorkers, bufferSamples: bufferSamples}
	for i := 0; i < maxWorkers; i++ {
		l := &lane{jobs: make(chan queuedJob), budget: NewSampleBudget(bufferSamples)}
		p.lanes = append(p.lanes, l)
		go l.run()
	}
	return p
}

func (l *lane) run() {
	for qj := range l.jobs {
		value, err := qj.job.Fn()
		qj.done <- result{value: value, err: err}
	}
}

func (p *Pool) laneFor(key string) *lane {
	if key != "" {
		var h uint64 = 1469598103934665603
		for _, c := range key {
			h ^= uint64(c)
			h *= 1099511628211
		}
		return p.lanes[int(h%uint64(len(p.lanes)))]
	}
	p.submitMu.Lock()
	l := p.lanes[p.rr%len(p.lanes)]
	p.rr++
	p.submitMu.Unlock()
	return l
}

func (p *Pool) Submit(job Job) *Future {
	l := p.laneFor(job.LaneKey)
	l.budget.Acquire(job.Samples)
	f := &Future{done: make(chan result, 1), samples: job.Samples, budget: l.budget}
	qj := queuedJob{job: job, done: f.done}
	l.jobs <- qj
	return f
}

func (p *Pool) Shutdown() {
	for _, lane := range p.lanes {
		close(lane.jobs)
	}
}

func (p *Pool) BufferCapacity() int { return p.bufferSamples }
func (p *Pool) MaxWorkers() int     { return p.maxWorkers }

type IndexedResult struct {
	Index int
	Value any
	Err   error
}

func MapOrdered(jobs []Job, maxWorkers, bufferSamples int, handle func(int, any) error) ([]error, error) {
	pool := NewPool(maxWorkers, bufferSamples)
	futures := make(chan *Future, len(jobs))
	go func() {
		for _, job := range jobs {
			futures <- pool.Submit(job)
		}
		close(futures)
	}()
	errsOut := make([]error, len(jobs))
	i := 0
	for future := range futures {
		value, err := future.Wait()
		if err != nil {
			errsOut[i] = err
		} else if handle != nil {
			if hErr := handle(i, value); hErr != nil {
				errsOut[i] = hErr
			}
		}
		future.Release()
		i++
	}
	pool.Shutdown()
	return errsOut, nil
}
