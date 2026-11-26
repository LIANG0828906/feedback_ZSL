package sched

import (
	"context"
	"fmt"
	"sync"
	"time"

	"dialoguefit/model"
)

type Config struct {
	MaxParallel        int `json:"max_parallel"`
	SampleBufferCap    int `json:"sample_buffer_cap"`
	TotalBufferSamples int `json:"total_buffer_samples,omitempty"`
	QueueBudget        int `json:"queue_budget"`
}

func DefaultConfig() Config {
	return Config{MaxParallel: 2, SampleBufferCap: 16384, QueueBudget: 2}
}

type Task struct {
	ID            string
	BufferSamples int
	Run           func(context.Context) error
}

type Result struct {
	ID  string
	Err error
}

type Pool struct {
	cfg Config

	mu         sync.Mutex
	bufferUsed int
	bufferCond *sync.Cond
	events     []model.QueueEvent
	now        func() time.Time
}

func New(cfg Config) *Pool {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = 1
	}
	if cfg.SampleBufferCap <= 0 {
		cfg.SampleBufferCap = 4096
	}
	if cfg.QueueBudget <= 0 {
		cfg.QueueBudget = 1
	}
	p := &Pool{cfg: cfg, now: time.Now}
	p.bufferCond = sync.NewCond(&p.mu)
	if p.cfg.TotalBufferSamples <= 0 {
		p.cfg.TotalBufferSamples = p.cfg.MaxParallel * p.cfg.SampleBufferCap
	}
	return p
}

func (p *Pool) Config() Config { return p.cfg }

func (p *Pool) Events() []model.QueueEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.QueueEvent(nil), p.events...)
}

func (p *Pool) Run(ctx context.Context, tasks []Task) []Result {
	if ctx == nil {
		ctx = context.Background()
	}
	jobs := make(chan taskEnvelope, p.cfg.QueueBudget)
	results := make([]Result, len(tasks))
	var wg sync.WaitGroup
	for worker := 0; worker < p.cfg.MaxParallel; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for envelope := range jobs {
				err := p.execute(ctx, envelope.task)
				results[envelope.index] = Result{ID: envelope.task.ID, Err: err}
			}
		}()
	}

	for i, task := range tasks {
		reason := ""
		if len(jobs) >= cap(jobs) {
			reason = "queue-budget-full"
		}
		start := p.now()
		select {
		case jobs <- taskEnvelope{task: task, index: i}:
			if reason != "" {
				p.record(task.ID, reason, time.Since(start))
			}
		case <-ctx.Done():
			results[i] = Result{ID: task.ID, Err: ctx.Err()}
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

type taskEnvelope struct {
	task  Task
	index int
}

func (p *Pool) execute(ctx context.Context, task Task) (err error) {
	need := task.BufferSamples
	if need < 0 {
		need = 0
	}
	if need > p.cfg.SampleBufferCap {
		return &model.Error{Code: model.CodeBudget, DialogueID: task.ID, Field: "buffer_samples",
			Message: fmt.Sprintf("task needs %d samples but per-thread buffer cap is %d", need, p.cfg.SampleBufferCap)}
	}

	start := p.now()
	waitedForBuffer := false
	p.mu.Lock()
	for p.bufferUsed+need > p.cfg.TotalBufferSamples {
		waitedForBuffer = true
		p.bufferCond.Wait()
	}
	p.bufferUsed += need
	p.mu.Unlock()
	if waitedForBuffer {
		p.record(task.ID, "worker-buffer-busy", time.Since(start))
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			err = model.StateError(task.ID, "panic", "task isolated: %v", recovered)
		}
		p.mu.Lock()
		p.bufferUsed -= need
		if p.bufferUsed < 0 {
			p.bufferUsed = 0
		}
		p.bufferCond.Broadcast()
		p.mu.Unlock()
	}()

	if task.Run == nil {
		return nil
	}
	return task.Run(ctx)
}

func (p *Pool) record(taskID, reason string, waited time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, model.QueueEvent{
		TaskID:   taskID,
		Reason:   reason,
		WaitMS:   waited.Milliseconds(),
		WaitUS:   waited.Microseconds(),
		AtUnixMS: p.now().UnixMilli(),
	})
}
