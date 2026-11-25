package engine

import (
	"sync"
	"sync/atomic"
	"time"

	"combatjudge/internal/model"
	"combatjudge/internal/store"
)

type GeomBuffer struct {
	floats []float64
	cap    int
}

func NewGeomBuffer(maxBytes int) *GeomBuffer {
	if maxBytes <= 0 {
		maxBytes = DefaultBufferBytes
	}
	capacity := maxBytes / 8
	if capacity < 1 {
		capacity = 0
	}
	return &GeomBuffer{floats: make([]float64, 0, capacity), cap: capacity}
}

func (b *GeomBuffer) Resize(n int) error {
	if n > b.cap {
		return model.NewError(model.CodeBudget, "geometry_buffer", "", "geometry buffer need %d floats, limit %d floats", n, b.cap)
	}
	b.floats = b.floats[:n]
	return nil
}

type jobOutput struct {
	value any
	err   error
}

type queuedJob struct {
	no   int
	kind string
	run  func(buf *GeomBuffer) (any, error)
	out  chan jobOutput
}

type GeometryJob struct {
	IntervalNo int
	Run        func(buf *GeomBuffer) (*GeometryResult, error)
}

type Pool struct {
	workers     int
	queue       chan queuedJob
	bufferBytes int
	waitsMu     sync.Mutex
	Waits       []store.QueueWaitRecord
	active      atomic.Int32
	MaxObserved atomic.Int32
	closed      atomic.Bool
}

func NewPool(workers, queueBudget, bufferBytes int) *Pool {
	if workers <= 0 {
		workers = DefaultWorkers
	}
	if queueBudget <= 0 {
		queueBudget = DefaultQueueBudget
	}
	p := &Pool{workers: workers, queue: make(chan queuedJob, queueBudget), bufferBytes: bufferBytes}
	for i := 0; i < workers; i++ {
		go p.worker()
	}
	return p
}

func (p *Pool) worker() {
	buf := NewGeomBuffer(p.bufferBytes)
	for q := range p.queue {
		current := p.active.Add(1)
		for {
			old := p.MaxObserved.Load()
			if current <= old || p.MaxObserved.CompareAndSwap(old, current) {
				break
			}
		}
		value, err := q.run(buf)
		p.active.Add(-1)
		q.out <- jobOutput{value: value, err: err}
	}
}

type Future struct {
	no   int
	kind string
	ch   chan jobOutput
}

func (p *Pool) SubmitGeometry(job GeometryJob) *Future {
	return p.submit(job.IntervalNo, "geometry", func(buf *GeomBuffer) (any, error) { return job.Run(buf) })
}

func (p *Pool) SubmitSettlement(intervalNo int, run func(buf *GeomBuffer) (*IntervalResult, error)) *Future {
	return p.submit(intervalNo, "settlement", func(buf *GeomBuffer) (any, error) { return run(buf) })
}

func (p *Pool) submit(no int, kind string, run func(*GeomBuffer) (any, error)) *Future {
	out := make(chan jobOutput, 1)
	q := queuedJob{no: no, kind: kind, run: run, out: out}
	start := time.Now()
	reason := ""
	select {
	case p.queue <- q:
	default:
		reason = "queue_budget_exhausted"
		p.queue <- q
	}
	waited := time.Since(start)
	if reason != "" || waited > 2*time.Millisecond {
		p.waitsMu.Lock()
		p.Waits = append(p.Waits, store.QueueWaitRecord{IntervalNo: no, Reason: firstReason(reason, kind+"_queue_wait"), WaitedMS: waited.Milliseconds()})
		p.waitsMu.Unlock()
	}
	return &Future{no: no, kind: kind, ch: out}
}

func (f *Future) Get() (*GeometryResult, error) {
	out := <-f.ch
	if out.err != nil {
		return nil, out.err
	}
	return out.value.(*GeometryResult), nil
}

func (f *Future) GetInterval() (*IntervalResult, error) {
	out := <-f.ch
	if out.err != nil {
		return nil, out.err
	}
	return out.value.(*IntervalResult), nil
}

func (p *Pool) Close() {
	if p.closed.CompareAndSwap(false, true) {
		close(p.queue)
	}
}

func (p *Pool) QueueWaits() []store.QueueWaitRecord {
	p.waitsMu.Lock()
	defer p.waitsMu.Unlock()
	return append([]store.QueueWaitRecord{}, p.Waits...)
}

func firstReason(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
