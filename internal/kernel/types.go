package kernel

import (
	"context"
	"sync"
	"time"

	"inputkernel/internal/model"
)

type NodeKind string

const (
	NodeDevice  NodeKind = "device"
	NodeAxis    NodeKind = "axis"
	NodeCurve   NodeKind = "curve"
	NodeBinding NodeKind = "binding"
	NodeTiming  NodeKind = "timing"
	NodeAction  NodeKind = "action"
)

type Node struct {
	ID           string   `json:"id"`
	Kind         NodeKind `json:"kind"`
	DeviceID     string   `json:"device_id,omitempty"`
	AxisID       string   `json:"axis_id,omitempty"`
	CurveID      string   `json:"curve_id,omitempty"`
	BindingID    string   `json:"binding_id,omitempty"`
	Dependencies []string `json:"dependencies"`
	ParamsHash   string   `json:"params_hash"`
	Level        int      `json:"level"`
}

type AxisObservation struct {
	EventSeq       int            `json:"event_seq"`
	TimeMS         int64          `json:"time_ms"`
	DeviceID       string         `json:"device_id"`
	AxisID         string         `json:"axis_id"`
	Raw            int64          `json:"raw"`
	BaseNormalized int64          `json:"base_normalized"`
	Residual       model.Fraction `json:"quantization_residual"`
	DeadzoneInside bool           `json:"deadzone_inside"`
	DeadzoneMargin int64          `json:"deadzone_margin"`
}

type AxisSample struct {
	EventSeq       int            `json:"event_seq"`
	TimeMS         int64          `json:"time_ms"`
	DeviceID       string         `json:"device_id"`
	BindingID      string         `json:"binding_id"`
	ActionID       string         `json:"action_id"`
	AxisID         string         `json:"axis_id"`
	Raw            int64          `json:"raw"`
	BaseNormalized int64          `json:"base_normalized"`
	Normalized     int64          `json:"normalized"`
	Residual       model.Fraction `json:"quantization_residual"`
	ResidualBound  int64          `json:"quantization_error_bound"`
	DeadzoneInside bool           `json:"deadzone_inside"`
	DeadzoneMargin int64          `json:"deadzone_margin"`
	CurveError     int64          `json:"curve_linearization_error"`
}

type ActionOutput struct {
	EventSeq      int               `json:"event_seq"`
	TriggerTimeMS int64             `json:"trigger_time_ms"`
	BindingID     string            `json:"binding_id"`
	ActionID      string            `json:"action_id"`
	DeviceID      string            `json:"device_id"`
	State         model.ActionState `json:"state"`
	AxisValue     *int64            `json:"axis_value,omitempty"`
	Residual      *model.Fraction   `json:"quantization_residual,omitempty"`
	CurveError    *int64            `json:"curve_linearization_error,omitempty"`
	Ordinal       int               `json:"ordinal"`
}

type TimingCheck struct {
	EventSeq  int    `json:"event_seq"`
	TimeMS    int64  `json:"time_ms"`
	BindingID string `json:"binding_id"`
	Kind      string `json:"kind"`
	MarginMS  int64  `json:"margin_ms"`
	WindowMS  int64  `json:"window_ms,omitempty"`
	EventGap  int64  `json:"event_gap_ms,omitempty"`
}

type ActionPayload struct {
	Outputs      []ActionOutput `json:"outputs"`
	AxisSamples  []AxisSample   `json:"axis_samples"`
	TimingChecks []TimingCheck  `json:"timing_checks"`
}

type NodeValue struct {
	AxisObservations []AxisObservation `json:"axis_observations,omitempty"`
	Action           *ActionPayload    `json:"action,omitempty"`
	Digest           string            `json:"digest,omitempty"`
}

type CacheEntry struct {
	NodeID       string    `json:"node_id"`
	Kind         NodeKind  `json:"kind"`
	CacheKey     string    `json:"cache_key"`
	ParamsHash   string    `json:"params_hash"`
	Dependencies []string  `json:"dependencies"`
	Value        NodeValue `json:"value"`
}

type Invalidation struct {
	NodeID string `json:"node_id"`
	Reason string `json:"reason"`
}

type InvalidationReport struct {
	ChangedNodes     []Invalidation `json:"changed_nodes"`
	ReusedNodes      int            `json:"reused_nodes"`
	FullInvalidation bool           `json:"full_invalidation"`
}

type Options struct {
	MaxWorkers      int
	PerWorkerBuffer int
}

func (o Options) normalized() Options {
	if o.MaxWorkers <= 0 {
		o.MaxWorkers = 1
	}
	if o.PerWorkerBuffer <= 0 {
		o.PerWorkerBuffer = 1
	}
	return o
}

type BufferFullRecord struct {
	TaskID    string `json:"task_id"`
	Worker    int    `json:"worker"`
	Reason    string `json:"reason"`
	WaitNanos int64  `json:"wait_nanos"`
}

type PoolStats struct {
	MaxWorkers        int                `json:"max_workers"`
	PerWorkerBuffer   int                `json:"per_worker_buffer"`
	Dispatched        int64              `json:"dispatched"`
	BufferFullWaits   int64              `json:"buffer_full_waits"`
	BufferFullRecords []BufferFullRecord `json:"buffer_full_records,omitempty"`
}

type Task struct {
	ID string
	Fn func(context.Context) error
}

type WorkerPool struct {
	ctx    context.Context
	cancel context.CancelFunc
	max    int
	buf    int
	chans  []chan Task
	wg     sync.WaitGroup

	batchMu sync.Mutex
	batchWG sync.WaitGroup

	statsMu sync.Mutex
	stats   PoolStats
	closed  bool
}

func NewWorkerPool(parent context.Context, options Options) *WorkerPool {
	options = options.normalized()
	ctx, cancel := context.WithCancel(parent)
	p := &WorkerPool{
		ctx: ctx, cancel: cancel,
		max: options.MaxWorkers, buf: options.PerWorkerBuffer,
		stats: PoolStats{MaxWorkers: options.MaxWorkers, PerWorkerBuffer: options.PerWorkerBuffer},
	}
	p.chans = make([]chan Task, p.max)
	for i := 0; i < p.max; i++ {
		p.chans[i] = make(chan Task, p.buf)
		p.wg.Add(1)
		go p.worker(i, p.chans[i])
	}
	return p
}

func (p *WorkerPool) worker(id int, ch <-chan Task) {
	defer p.wg.Done()
	for task := range ch {
		err := p.runTask(task)
		if err != nil {
			p.cancel()
		}
		p.batchWG.Done()
	}
}

func (p *WorkerPool) runTask(task Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{value: r}
		}
	}()
	return task.Fn(p.ctx)
}

type panicError struct{ value interface{} }

func (e *panicError) Error() string { return "worker panic: " + toString(e.value) }

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return "unknown"
}

// Run dispatches one deterministic wave. If a worker queue is full, dispatch
// blocks until capacity is available and records why it had to wait.
func (p *WorkerPool) Run(tasks []Task) error {
	p.batchMu.Lock()
	defer p.batchMu.Unlock()
	if len(tasks) == 0 {
		return nil
	}
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	p.batchWG.Add(len(tasks))
	done := make(chan struct{})
	go func() {
		p.batchWG.Wait()
		close(done)
	}()
	sent := 0
	for i, task := range tasks {
		worker := i % p.max
		start := time.Now()
		select {
		case p.chans[worker] <- task:
			p.recordDispatch()
			sent++
		default:
			select {
			case p.chans[worker] <- task:
				p.recordBlocked(task.ID, worker, time.Since(start))
				sent++
			case <-p.ctx.Done():
				for j := sent; j < len(tasks); j++ {
					p.batchWG.Done()
				}
				<-done
				return p.ctx.Err()
			}
		}
	}
	select {
	case <-done:
		return p.ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

func (p *WorkerPool) recordDispatch() {
	p.statsMu.Lock()
	p.stats.Dispatched++
	p.statsMu.Unlock()
}

func (p *WorkerPool) recordBlocked(id string, worker int, wait time.Duration) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	p.stats.BufferFullWaits++
	if len(p.stats.BufferFullRecords) < 1000 {
		p.stats.BufferFullRecords = append(p.stats.BufferFullRecords, BufferFullRecord{
			TaskID: id, Worker: worker, Reason: "per_worker_buffer_full", WaitNanos: wait.Nanoseconds(),
		})
	}
}

func (p *WorkerPool) Stats() PoolStats {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	out := p.stats
	out.BufferFullRecords = append([]BufferFullRecord(nil), p.stats.BufferFullRecords...)
	return out
}

func (p *WorkerPool) ResetStats() {
	p.statsMu.Lock()
	p.stats = PoolStats{MaxWorkers: p.max, PerWorkerBuffer: p.buf}
	p.statsMu.Unlock()
}

func (p *WorkerPool) Close() {
	p.statsMu.Lock()
	if p.closed {
		p.statsMu.Unlock()
		return
	}
	p.closed = true
	p.statsMu.Unlock()
	for _, ch := range p.chans {
		close(ch)
	}
	p.wg.Wait()
	p.cancel()
}
